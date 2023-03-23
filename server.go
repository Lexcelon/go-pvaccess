package pvaccess

import (
	//"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"time"
	"bytes"

	"github.com/Lexcelon/go-pvaccess/internal/connection"
	"github.com/Lexcelon/go-pvaccess/internal/ctxlog"
	"github.com/Lexcelon/go-pvaccess/internal/proto"
	"github.com/Lexcelon/go-pvaccess/internal/search"
	"github.com/Lexcelon/go-pvaccess/internal/server/monitor"
	"github.com/Lexcelon/go-pvaccess/internal/server/status"
	"github.com/Lexcelon/go-pvaccess/pvdata"
	"golang.org/x/sync/errgroup"
)

type Server struct {
	DisableSearch bool

	search *search.Server
	ln     net.Listener

	mu               sync.RWMutex
	channelProviders []ChannelProvider
}

const udpAddr = ":5076"

// TODO: Use this port if it's available.
const tcpAddr = ":5075"

func NewServer() (*Server, error) {
	s := &Server{}
	s.channelProviders = []ChannelProvider{&status.Channel{s}}
	return s, nil
}

// ListenAndServe listens on a random port and then calls Serve.
func (srv *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", "")
	if err != nil {
		return err
	}
	return srv.Serve(ctx, ln)
}

// Serve runs a PVAccess server on l until the context is cancelled.
func (srv *Server) Serve(ctx context.Context, l net.Listener) error {
	srv.search = &search.Server{
		ServerAddr: l.Addr().(*net.TCPAddr),
		Server:     srv,
	}
	srv.ln = l
	ctxlog.L(ctx).Infof("PVAccess server listening on %v", srv.ln.Addr())
	var g errgroup.Group
	g.Go(func() error {
		<-ctx.Done()
		ctxlog.L(ctx).Infof("PVAccess server shutting down")
		return srv.ln.Close()
	})
	if !srv.DisableSearch {
		g.Go(func() error {
			if err := srv.search.Serve(ctx); err != nil {
				ctxlog.L(ctx).Errorf("failed to serve search requests: %v", err)
				return err
			}
			return nil
		})
	}
	g.Go(func() error {
		for {
			conn, err := srv.ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Temporary() {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				return err
			}
			g.Go(func() error {
				srv.handleConnection(ctx, conn)
				return nil
			})
		}
	})
	return g.Wait()
}

func (s *Server) AddChannelProvider(provider ChannelProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channelProviders = append(s.channelProviders, provider)
}

func (s *Server) ChannelProviders() []ChannelProvider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ChannelProvider{}, s.channelProviders...)
}

type serverConn struct {
	*connection.Connection
	srv *Server
	g   *errgroup.Group

	mu       sync.Mutex
	channels map[pvdata.PVInt]Channel
	requests map[pvdata.PVInt]*request
}

type connChannel struct {
	name      string
	handleRPC func(ctx context.Context, args pvdata.PVStructure) (response interface{}, err error)
}

type requestStatus int

const (
	INIT = iota
	READY
	REQUEST_IN_PROGRESS
	CANCELLED
	DESTROYED
)

var requestStatusNames = map[requestStatus]string{
	0: "INIT",
	1: "READY",
	2: "REQUEST_IN_PROGRESS",
	3: "CANCELLED",
	4: "DESTROYED",
}

func (r requestStatus) String() string {
	return requestStatusNames[r]
}

type request struct {
	doer   interface{}
	cancel func()
	status requestStatus
}

func (c *serverConn) addRequest(id pvdata.PVInt, r *request) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.requests[id]; ok {
		if existing.status != DESTROYED {
			return fmt.Errorf("request ID %x already exists with status %s", id, requestStatusNames[existing.status])
		}
	}
	c.requests[id] = r
	return nil
}

func (c *serverConn) cancelRequestLocked(id pvdata.PVInt) error {
	if existing, ok := c.requests[id]; ok {
		if existing.status < CANCELLED {
			existing.status = CANCELLED
		}
		if existing.cancel != nil {
			existing.cancel()
			existing.cancel = nil
		}
		return nil
	}
	return fmt.Errorf("unknown request %d", id)
}

func (c *serverConn) destroyRequestLocked(id pvdata.PVInt) error {
	if existing, ok := c.requests[id]; ok {
		if existing.status < DESTROYED {
			existing.status = DESTROYED
		}
		if existing.cancel != nil {
			existing.cancel()
			existing.cancel = nil
		}
		delete(c.requests, id)
		return nil
	}
	return fmt.Errorf("unknown request %d", id)
}

func (srv *Server) newConn(conn io.ReadWriter) *serverConn {
	c := connection.New(conn, proto.FLAG_FROM_SERVER)
	return &serverConn{
		Connection: c,
		srv:        srv,
		channels:   make(map[pvdata.PVInt]Channel),
		requests:   make(map[pvdata.PVInt]*request),
	}
}

func (srv *Server) handleConnection(ctx context.Context, conn net.Conn) {
	ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
		"local_addr":  srv.ln.Addr(),
		"remote_addr": conn.RemoteAddr(),
		"proto":       "tcp",
	})
	c := srv.newConn(conn)
	g, ctx := errgroup.WithContext(ctx)
	c.g = g
	g.Go(func() error {
		<-ctx.Done()
		// TODO: Gracefully destroy requests and channels before closing?
		return conn.Close()
	})
	g.Go(func() error {
		ctxlog.L(ctx).Infof("new connection")
		return c.serve(ctx)
	})
	if err := g.Wait(); err != nil {
		ctxlog.L(ctx).Errorf("error on connection: %v", err)
	}
}

func (c *serverConn) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	// Is this the right version? Changing it to 7 for now.
	// c.Version = pvdata.PVByte(2)
	c.Version = pvdata.PVByte(7)

	// 0 = Ignore byte order field in header
	if err := c.SendCtrl(ctx, proto.CTRL_SET_BYTE_ORDER, 0); err != nil {
		return err
	}
	recieveBufferSize := c.ReceiveBufferSize()
	fmt.Println("recieveBufferSize:", recieveBufferSize)
	req := proto.ConnectionValidationRequest{
		ServerReceiveBufferSize:            pvdata.PVInt(recieveBufferSize),
		ServerIntrospectionRegistryMaxSize: 0x7fff,
		AuthNZ:                             []string{"anonymous"},
	}
	c.SendApp(ctx, proto.APP_CONNECTION_VALIDATION, &req)

	for {
		if err := c.handleServerOnePacket(ctx); err != nil {
			if err == io.EOF {
				cancel()
				// TODO: Cleanup resources (requests, channels, etc.)
				ctxlog.L(ctx).Infof("client went away, closing connection")
				return nil
			}
			return err
		}
	}
}
func (c *serverConn) handleServerOnePacket(ctx context.Context) error {
	fmt.Println("handleServerOnePacket----------------")
	msg, err := c.Next(ctx)
	if err != nil {
		return err
	}
	if f, ok := serverDispatch[msg.Header.MessageCommand]; ok {
		return f(c, ctx, msg)
	} else {
		ctxlog.L(ctx).Errorf("no handler for command 0x%x", msg.Header.MessageCommand)
	}
	return nil
}

var serverDispatch = map[pvdata.PVByte]func(c *serverConn, ctx context.Context, msg *connection.Message) error{
	proto.APP_CONNECTION_VALIDATION: (*serverConn).handleConnectionValidation,
	proto.APP_CHANNEL_CREATE:        (*serverConn).handleCreateChannelRequest,
	proto.APP_CHANNEL_DESTROY:       (*serverConn).handleChannelDestroy,
	proto.APP_CHANNEL_GET:           (*serverConn).handleChannelGet,
	proto.APP_CHANNEL_PUT:           (*serverConn).handleChannelPut,
	proto.APP_CHANNEL_RPC:           (*serverConn).handleChannelRPC,
	proto.APP_CHANNEL_MONITOR:       (*serverConn).handleChannelMonitor,
	proto.APP_REQUEST_CANCEL:        (*serverConn).handleRequestCancelDestroy,
	proto.APP_REQUEST_DESTROY:       (*serverConn).handleRequestCancelDestroy,
	proto.APP_SEARCH_REQUEST:        (*serverConn).handleSearchRequest,
}

func (c *serverConn) handleConnectionValidation(ctx context.Context, msg *connection.Message) error {
	// Dump message in hex:
	msghex := ""
	for _, b := range msg.Data {
		msghex += fmt.Sprintf("%02x ", b)
	}
	fmt.Println("12345678: " + msghex)
	var resp proto.ConnectionValidationResponse
	if err := msg.Decode(&resp); err != nil {
		return err
	}
	ctxlog.L(ctx).Infof("received connection validation %#v", resp)
	// TODO: Implement flow control
	return c.SendApp(ctx, proto.APP_CONNECTION_VALIDATED, &proto.ConnectionValidated{})
}

func (c *serverConn) handleCreateChannelRequest(ctx context.Context, msg *connection.Message) error {
	// Dump message in hex:
	msghex := ""
	for _, b := range msg.Data {
		msghex += fmt.Sprintf("%02x ", b)
	}
	fmt.Println("12345678: " + msghex)
	var req proto.CreateChannelRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	var resp proto.CreateChannelResponse
	if len(req.Channels) == 1 {
		ch := req.Channels[0]
		ctxlog.L(ctx).Infof("received request to create channel %q as client channel ID %x", ch.ChannelName, ch.ClientChannelID)
		resp.ClientChannelID = ch.ClientChannelID
		channel, err := c.createChannel(ctx, ch.ClientChannelID, ch.ChannelName)
		if err != nil {
			resp.Status = errorToStatus(err)
		} else if channel != nil {
			resp.ServerChannelID = ch.ClientChannelID
		} else {
			resp.Status.Type = pvdata.PVStatus_ERROR
			resp.Status.Message = pvdata.PVString(fmt.Sprintf("unknown channel %q", ch.ChannelName))
		}
		ctxlog.L(ctx).Infof("channel status = %v", resp.Status)
	} else {
		resp.Status.Type = pvdata.PVStatus_ERROR
		resp.Status.Message = "wrong number of channels"
	}
	return c.SendApp(ctx, proto.APP_CHANNEL_CREATE, &resp)
}

func (c *serverConn) handleChannelDestroy(ctx context.Context, msg *connection.Message) error {
	// Dump message in hex:
	msghex := ""
	for _, b := range msg.Data {
		msghex += fmt.Sprintf("%02x ", b)
	}
	fmt.Println("12345678: " + msghex)
	var req proto.DestroyChannel
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Infof("CHANNEL_DESTROY(%d, %d)", req.ServerChannelID, req.ClientChannelID)
	if req.ServerChannelID != req.ClientChannelID {
		// TODO: Spec says we "MUST respond with an error status", but response struct doesn't contain a status code...
		// Returning nil will just cause the client to time out.
		return nil
	}
	if err := c.destroyChannel(req.ServerChannelID); err != nil {
		ctxlog.L(ctx).Errorf("destroying channel: %v", err)
	}
	// Response is just a copy of the request.
	return c.SendApp(ctx, proto.APP_CHANNEL_DESTROY, &req)
}

func errorToStatus(err error) pvdata.PVStatus {
	if err == nil {
		return pvdata.PVStatus{}
	}
	if s, ok := err.(pvdata.PVStatus); ok {
		return s
	}
	return pvdata.PVStatus{
		Type:    pvdata.PVStatus_FATAL,
		Message: pvdata.PVString(err.Error()),
	}
}

func (c *serverConn) getChannel(ctx context.Context, id pvdata.PVInt) (Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	channel := c.channels[id]
	if channel == nil {
		return nil, fmt.Errorf("unknown channel ID %x", id)
	}
	ctxlog.L(ctx).Debugf("channel = %#v", channel)
	return channel, nil
}

func (c *serverConn) handleChannelGet(ctx context.Context, msg *connection.Message) error {
	// Dump message in hex:
	msghex := ""
	for _, b := range msg.Data {
		msghex += fmt.Sprintf("%02x ", b)
	}
	fmt.Println("12345678: " + msghex)
	

	
	
	var req proto.ChannelGetRequest
	// fmt.Println("CHANNEL_GET", msg)
	
	if len(msg.Data) >= 11 && msg.Data[9] == 0xfe && msg.Data[8] == 0x08 {
		fmt.Println("got a cached type descriptor, but we don't handle that, so manually modifying message")
		msg.Data[9] = 0xfd
		msg.Data[10] = 0x01
		msg.Data[11] = 0x00
		msg.Data= append(msg.Data,0x80)
		msg.Data= append(msg.Data,0x00)
		msg.Data= append(msg.Data,0x00)

		
	}
	
	if err := msg.Decode(&req); err != nil {
		return err
	}

	ctxlog.L(ctx).Debugf("CHANNEL_GET(%#v)", req)
	c.g.Go(func() (err error) {
		defer func() {
			if err != nil {
				ctxlog.L(ctx).Warnf("Channel Get failed: %v", err)
				err = c.SendApp(ctx, proto.APP_CHANNEL_GET, &proto.ChannelResponseError{
					RequestID:  req.RequestID,
					Subcommand: req.Subcommand,
					Status:     errorToStatus(err),
				})
			}
		}()
		channel, err := c.getChannel(ctx, req.ServerChannelID)
		if err != nil {
			return err
		}
		ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
			"channel":    channel.Name(),
			"channel_id": req.ServerChannelID,
			"request_id": req.RequestID,
		})
		switch req.Subcommand {
		case proto.CHANNEL_GET_INIT:
			args, ok := req.PVRequest.Data.(pvdata.PVStructure)
			if !ok {
				return fmt.Errorf("Get arguments were of type %T, expected PVStructure", req.PVRequest.Data)
			}

			ctxlog.L(ctx).Printf("received request to init channel get with body %v", args)
			// TODO: Parse args to select output data
			var geter ChannelGeter
			if getc, ok := channel.(ChannelGetCreator); ok {
				var err error
				geter, err = getc.CreateChannelGet(ctx, args)
				if err != nil {
					return err
				}
			} else if g, ok := channel.(ChannelGeter); ok {
				geter = g
			} else {
				return fmt.Errorf("channel %q (ID %x) does not support Get", channel.Name(), req.ServerChannelID)
			}
			if err := c.addRequest(req.RequestID, &request{doer: geter, status: READY}); err != nil {
				return err
			}
			// TODO: Optional interface to get field description without having to do expensive get
			out, err := geter.ChannelGet(ctx)
			if err != nil {
				return err
			}
			o := reflect.ValueOf(out).Elem()
			fmt.Println("out:", reflect.ValueOf(o))
			pvs, err := pvdata.NewPVStructure(out)
			if err != nil {
				return err
			}
			fmt.Println("pvs:", reflect.ValueOf(pvs), reflect.TypeOf(pvs), pvs.String())
			fd, err := pvs.FieldDesc()
			if err != nil {
				return err
			}
			return c.SendApp(ctx, proto.APP_CHANNEL_GET, &proto.ChannelGetResponseInit{
				RequestID:     req.RequestID,
				Subcommand:    req.Subcommand,
				PVStructureIF: fd,
			})
		default:
			ctxlog.L(ctx).Printf("received request to execute channel get")
			c.mu.Lock()
			defer c.mu.Unlock()
			r := c.requests[req.RequestID]
			if r.status != READY {
				return pvdata.PVStatus{
					Type:    pvdata.PVStatus_ERROR,
					Message: pvdata.PVString("request not READY"),
				}
			}
			geter, ok := r.doer.(ChannelGeter)
			if !ok {
				return errors.New("request not for get")
			}
			ctx, cancel := context.WithCancel(ctx)
			r.status = REQUEST_IN_PROGRESS
			r.cancel = cancel
			c.g.Go(func() error {
				respData, err := geter.ChannelGet(ctx)
				resp := &proto.ChannelGetResponse{
					RequestID:  req.RequestID,
					Subcommand: req.Subcommand,
					Status:     errorToStatus(err),
					Value: pvdata.PVStructureDiff{
						Value: respData,
					},
				}
				if err := c.SendApp(ctx, proto.APP_CHANNEL_GET, resp); err != nil {
					ctxlog.L(ctx).Errorf("sending get response: %v", err)
				}

				c.mu.Lock()
				defer c.mu.Unlock()
				r.status = READY
				if req.Subcommand&proto.CHANNEL_GET_DESTROY == proto.CHANNEL_GET_DESTROY {
					r.status = DESTROYED
					delete(c.requests, req.RequestID)
				}
				return nil
			})
		}
		return nil
	})
	return nil
}
func (c *serverConn) handleChannelPut(ctx context.Context, msg *connection.Message) error {
	// Dump message in hex:
	msghex := ""
	for _, b := range msg.Data {
		msghex += fmt.Sprintf("%02x ", b)
	}
	fmt.Println("12345678: " + msghex)
	// debug.PrintStack()
	var req proto.ChannelPutRequest
	ctxlog.L(ctx).Debugf("BEGIN_CHANNEL_PUT (%+v)", msg)
	fmt.Println("BEGIN CHANNEL_PUT\n", msg)

	if err := msg.Decode(&req); err != nil {
		fmt.Println("FAILED MESSAGE DECODE", err)
		return err
	}
	
	ctxlog.L(ctx).Debugf("CHANNEL_PUT(%#v)", req)
	c.g.Go(func() (err error) {
		defer func() {
			if err != nil {
				ctxlog.L(ctx).Warnf("Channel Put failed: %v", err)
				err = c.SendApp(ctx, proto.APP_CHANNEL_PUT, &proto.ChannelResponseError{
					RequestID:  req.RequestID,
					Subcommand: req.Subcommand,
					Status:     errorToStatus(err),
				})
			}
		}()
		channel, err := c.getChannel(ctx, req.ServerChannelID)
		if err != nil {
			return err
		}
		ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
			"channel":    channel.Name(),
			"channel_id": req.ServerChannelID,
			"request_id": req.RequestID,
		})
		switch req.Subcommand {
		case proto.CHANNEL_PUT_INIT:
			ctxlog.L(ctx).Debugf("CHANNEL_PUT_INIT(%#v)", req)
			var err error

			fd := pvdata.StructFieldDesc{
				Name:  "structure",
				Field: pvdata.FieldDesc{TypeCode: pvdata.FLOAT},
			}
			if err != nil {
				return err
			}

			// ctxlog.L(ctx).Printf("received request to init channel put with body %v", args)
			var puter ChannelPuter
			if getc, ok := channel.(ChannelPutCreator); ok {
				var err error
				puter, err = getc.CreateChannelPut(ctx, fd)
				if err != nil {
					return err
				}
			} else if p, ok := channel.(ChannelPuter); ok {
				puter = p
			} else {
				return fmt.Errorf("channel %q (ID %x) does not support Put", channel.Name(), req.ServerChannelID)
			}
			if err := c.addRequest(req.RequestID, &request{doer: puter, status: READY}); err != nil {
				return err
			}

			if err != nil {
				return err
			}
			
			
			
			
			//but we still need to get the channel's field descriptor.  So do that like we did in GET
			//var reqq proto.ChannelGetRequest
			//if err := msg.Decode(&reqq); err != nil {
			//	fmt.Println("FAILED MESSAGE DECODE", err)
			//	return err
			//}
			//args := reqq.PVRequest.Data.(pvdata.PVStructure)
			//var geter ChannelGeter
			//if getc, ok := channel.(ChannelGetCreator); ok {
			//	var err error
			//	geter, err = getc.CreateChannelGet(ctx,args)
			//	if err != nil {
			//		return err
			//	}
			//} else if g, ok := channel.(ChannelGeter); ok {
			//	geter = g
			//} else {
			//	return fmt.Errorf("channel %q (ID %x) does not support Get", channel.Name(), req.ServerChannelID)
			//}
			
			//out,err := geter.ChannelGet(ctx)
			//pvs,err := pvdata.NewPVStructure(out)
			//fdd,err := pvs.FieldDesc()
			
			
			//actually send the response
			fdd,err := channel.FieldDesc()

			return c.SendApp(ctx, proto.APP_CHANNEL_PUT, &proto.ChannelPutResponseInit{
				RequestID:        req.RequestID,
				Subcommand:       req.Subcommand,
				//Status:           errorToStatus(err), //this is populated by default
				PVPutStructureIF: fdd,
			})
		case proto.CHANNEL_PUT_GET_PUT:
			ctxlog.L(ctx).Debugf("CHANNEL_PUT_GET_PUT(%#v)", req)

			ctxlog.L(ctx).Printf("received request to execute channel get-put with body %v", req.ToPutBitSet)
			c.mu.Lock()
			defer c.mu.Unlock()
			r := c.requests[req.RequestID]
			if r.status != READY {
				return pvdata.PVStatus{
					Type:    pvdata.PVStatus_ERROR,
					Message: pvdata.PVString("request not READY"),
				}
			}
			ctx, cancel := context.WithCancel(ctx)
			r.status = REQUEST_IN_PROGRESS
			r.cancel = cancel
			c.g.Go(func() error {
				var geter ChannelGeter
				if getc, ok := channel.(ChannelGetCreator); ok {
					var err error
					var pvstrcut pvdata.PVStructure
					pvstrcut, err = pvdata.NewPVStructure(nil)
					if err != nil {
						return err
					}
					geter, err = getc.CreateChannelGet(ctx, pvstrcut)
					if err != nil {
						return err
					}
				} else if g, ok := channel.(ChannelGeter); ok {
					geter = g
				} else {
					return fmt.Errorf("channel %q (ID %x) does not support get-put", channel.Name(), req.ServerChannelID)
				}
				respData, err := geter.ChannelGet(ctx)
				resp := &proto.ChannelGetPutResponse{
					RequestID:  req.RequestID,
					Subcommand: req.Subcommand,
					Status:     errorToStatus(err),
					Value: pvdata.PVStructureDiff{
						Value: respData,
					},
				}
				if err := c.SendApp(ctx, proto.APP_CHANNEL_PUT, resp); err != nil {
					ctxlog.L(ctx).Errorf("sending put response: %v", err)
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				r.status = READY
				if req.Subcommand&proto.CHANNEL_PUT_DESTROY == proto.CHANNEL_PUT_DESTROY {
					r.status = DESTROYED
					delete(c.requests, req.RequestID)
				}
				return nil
			})
		default:
			ctxlog.L(ctx).Debugf("CHANNEL_PUT_DEFAULT(%#v)", req)
			
			//we need to re-decode this because the data structure is different from put init and getput
			
			//var reqq proto.ChannelPutRequestPut

			//if err := msg.Decode(&reqq); err != nil {
			//	fmt.Println("FAILED MESSAGE DECODE PUT DEFAULT", err)
			//	return err
			//}
			
			//ctxlog.L(ctx).Debugf("CHANNEL_PUT_DEFAULT(%#v)", reqq)
			
			
			//ctxlog.L(ctx).Printf("received request to execute channel put with body %v", reqq.ToPutBitSet)
			c.mu.Lock()
			defer c.mu.Unlock()
			r := c.requests[req.RequestID]
			if r.status != READY {
				return pvdata.PVStatus{
					Type:    pvdata.PVStatus_ERROR,
					Message: pvdata.PVString("request not READY"),
				}
			}
			
			//throw an error if the data is not a long
			
			//byte

			

			
			 puter, ok := r.doer.(ChannelPuter)
			 if !ok {
			 	return errors.New("request not for put")
			 }
			ctx, cancel := context.WithCancel(ctx)
			r.status = REQUEST_IN_PROGRESS
			r.cancel = cancel
			c.g.Go(func() error {
				// field := req.PVPutStructureData

				/// TODO : decode the data using the structure from the request
				//decoder := pvdata.DecoderState{
				//	Buf:       bytes.NewReader([]byte{1, 3}),
				//	ByteOrder: binary.BigEndian,
				//}
				//var data pvdata.PVAny
				//err := pvdata.Decode(&decoder, data)
				
				if len(msg.Data) == 12 {
					mydata := uint8(msg.Data[11])
					fmt.Println("mydata:",mydata)
					pv := pvdata.PVUByte(mydata)
					puter.ChannelPut(&pv, ctx)
				} 
				if len(msg.Data) == 13 {
					mydata := uint16(binary.LittleEndian.Uint16(msg.Data[11:13]))
					fmt.Println("mydata:",mydata)
					pv := pvdata.PVUShort(mydata)
					puter.ChannelPut(&pv, ctx)
				} 
				if len(msg.Data) == 15 {
					var mydata float32
					if err := binary.Read(bytes.NewReader(msg.Data[11:15]),binary.LittleEndian,&mydata); err != nil {
						fmt.Println(err)
						return err
					}
					fmt.Println("mydata:",mydata)
					pv := pvdata.PVFloat(mydata)
					puter.ChannelPut(&pv, ctx)
				}
				if len(msg.Data) == 19 {
					mydata := uint64(binary.LittleEndian.Uint64(msg.Data[11:19]))
					fmt.Println("mydata:",mydata)
					pv := pvdata.PVLong(mydata)
					puter.ChannelPut(&pv, ctx)
				} 
				
				if len(msg.Data) < 12 || len(msg.Data) > 19 {
					return errors.New("PUT support is not implemented for this data type")
				}
				
				
				
				
				 

				resp := &proto.ChannelPutResponse{
					RequestID:  req.RequestID,
					Subcommand: req.Subcommand,
					Status:     errorToStatus(err),
				}
				if err := c.SendApp(ctx, proto.APP_CHANNEL_PUT, resp); err != nil {
					ctxlog.L(ctx).Errorf("sending put response: %v", err)
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				r.status = READY
				if req.Subcommand&proto.CHANNEL_PUT_DESTROY == proto.CHANNEL_PUT_DESTROY {
					r.status = DESTROYED
					delete(c.requests, req.RequestID)
				}
				return nil
			})
		}
		return nil
	})

	fmt.Println("END OF CHANNEL_PUT")
	return nil
}
func (c *serverConn) handleChannelMonitor(ctx context.Context, msg *connection.Message) error {
	var req proto.ChannelMonitorRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Debugf("CHANNEL_MONITOR(%#v)", req)
	c.g.Go(func() (err error) {
		defer func() {
			if err != nil {
				ctxlog.L(ctx).Warnf("Channel Monitor failed: %v", err)
				err = c.SendApp(ctx, proto.APP_CHANNEL_MONITOR, &proto.ChannelResponseError{
					RequestID:  req.RequestID,
					Subcommand: pvdata.PVByte(req.Subcommand),
					Status:     errorToStatus(err),
				})
			}
		}()
		channel, err := c.getChannel(ctx, req.ServerChannelID)
		if err != nil {
			return err
		}
		ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
			"channel":    channel.Name(),
			"channel_id": req.ServerChannelID,
			"request_id": req.RequestID,
		})
		if req.Subcommand&proto.CHANNEL_MONITOR_INIT == proto.CHANNEL_MONITOR_INIT {
			args, ok := req.PVRequest.Data.(pvdata.PVStructure)
			if !ok {
				return fmt.Errorf("Monitor arguments were of type %T, expected PVStructure", req.PVRequest.Data)
			}
			ctxlog.L(ctx).Printf("received request to init channel monitor with body %v", args)
			// TODO: Parse args to select output data
			var nexter Nexter
			if nextc, ok := channel.(ChannelMonitorCreator); ok {
				nexter, err = nextc.CreateChannelMonitor(ctx, args)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("channel %q (ID %x) does not support Monitor", channel.Name(), req.ServerChannelID)
			}
			value, err := nexter.Next(ctx)
			if err != nil {
				return err
			}
			m := monitor.New(ctx, args, nexter, func(value interface{}) {
				c.SendApp(ctx, proto.APP_CHANNEL_MONITOR, &proto.ChannelMonitorResponse{
					RequestID: req.RequestID,
					Value: pvdata.PVStructureDiff{
						Value: value,
					},
				})
			})
			m.Ack(ctx, int(req.NFree))
			// TODO: Use QueueSize to initialize pipeline support
			if err := c.addRequest(req.RequestID, &request{doer: m, status: READY}); err != nil {
				return err
			}
			pvs, err := pvdata.NewPVStructure(value)
			if err != nil {
				return err
			}
			fd, err := pvs.FieldDesc()
			if err != nil {
				return err
			}
			if err := c.SendApp(ctx, proto.APP_CHANNEL_MONITOR, &proto.ChannelMonitorResponseInit{
				RequestID:     req.RequestID,
				Subcommand:    proto.CHANNEL_MONITOR_INIT,
				PVStructureIF: fd,
			}); err != nil {
				return err
			}
			m.Send(ctx, value)
			return nil
		}
		ctxlog.L(ctx).Printf("received request on existing monitor")
		c.mu.Lock()
		defer c.mu.Unlock()
		r := c.requests[req.RequestID]
		if r.status != READY {
			return pvdata.PVStatus{
				Type:    pvdata.PVStatus_ERROR,
				Message: pvdata.PVString("request not READY"),
			}
		}
		m, ok := r.doer.(*monitor.Monitor)
		if !ok {
			return errors.New("request not for monitor")
		}
		// If CHANNEL_MONITOR_PIPELINE_SUPPORT, add NFree to window (what to do with QueueSize?)
		if req.Subcommand&proto.CHANNEL_MONITOR_PIPELINE_SUPPORT == proto.CHANNEL_MONITOR_PIPELINE_SUPPORT {
			m.Ack(ctx, int(req.NFree))
		}
		// If CHANNEL_MONITOR_SUBSCRIPTION, set running to r.Subcommand & CHANNEL_MONITOR_SUBSCRIPTION_RUN
		if req.Subcommand&proto.CHANNEL_MONITOR_SUBSCRIPTION == proto.CHANNEL_MONITOR_SUBSCRIPTION {
			if req.Subcommand&proto.CHANNEL_MONITOR_SUBSCRIPTION_RUN == proto.CHANNEL_MONITOR_SUBSCRIPTION_RUN {
				m.Start(ctx)
			} else {
				m.Stop(ctx)
			}
		}
		// If CHANNEL_MONITOR_TERMINATE, destroy request
		if req.Subcommand&proto.CHANNEL_MONITOR_TERMINATE == proto.CHANNEL_MONITOR_TERMINATE {
			if err := m.Terminate(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	return nil
}

func (c *serverConn) handleChannelRPC(ctx context.Context, msg *connection.Message) error {
	var req proto.ChannelRPCRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Debugf("CHANNEL_RPC(%#v)", req)
	c.g.Go(func() error {
		return c.handleChannelRPCBody(ctx, req)
	})
	return nil
}

func (c *serverConn) handleChannelRPCBody(ctx context.Context, req proto.ChannelRPCRequest) (err error) {
	resp := &proto.ChannelRPCResponseInit{
		RequestID:  req.RequestID,
		Subcommand: req.Subcommand,
	}
	defer func() {
		if err != nil {
			ctxlog.L(ctx).Warnf("Channel RPC failed: %v", err)
			resp.Status = errorToStatus(err)
			err = c.SendApp(ctx, proto.APP_CHANNEL_RPC, resp)
		}
	}()
	channel, err := c.getChannel(ctx, req.ServerChannelID)
	if err != nil {
		return err
	}
	ctx = ctxlog.WithFields(ctx, ctxlog.Fields{
		"channel":    channel.Name(),
		"channel_id": req.ServerChannelID,
		"request_id": req.RequestID,
	})
	args, ok := req.PVRequest.Data.(pvdata.PVStructure)
	if !ok {
		return fmt.Errorf("RPC arguments were of type %T, expected PVStructure", req.PVRequest.Data)
	}
	switch req.Subcommand {
	case proto.CHANNEL_RPC_INIT:
		ctxlog.L(ctx).Printf("received request to init channel RPC with body %v", args)
		var rpcer ChannelRPCer
		if rpcc, ok := channel.(ChannelRPCCreator); ok {
			var err error
			rpcer, err = rpcc.CreateChannelRPC(ctx, args)
			if err != nil {
				return err
			}
		} else if r, ok := channel.(ChannelRPCer); ok {
			rpcer = r
		} else {
			return fmt.Errorf("channel %q (ID %x) does not support RPC", channel.Name(), req.ServerChannelID)
		}
		if err := c.addRequest(req.RequestID, &request{doer: rpcer, status: READY}); err != nil {
			return err
		}
		return c.SendApp(ctx, proto.APP_CHANNEL_RPC, resp)
	default:
		ctxlog.L(ctx).Printf("received request to execute channel RPC with body %v", args)
		c.mu.Lock()
		defer c.mu.Unlock()
		r := c.requests[req.RequestID]
		if r.status != READY {
			return pvdata.PVStatus{
				Type:    pvdata.PVStatus_ERROR,
				Message: pvdata.PVString("request not READY"),
			}
		}
		rpcer, ok := r.doer.(ChannelRPCer)
		if !ok {
			return errors.New("request not for RPC")
		}
		ctx, cancel := context.WithCancel(ctx)
		r.status = REQUEST_IN_PROGRESS
		r.cancel = cancel
		c.g.Go(func() error {
			respData, err := rpcer.ChannelRPC(ctx, args)
			resp := &proto.ChannelRPCResponse{
				RequestID:      req.RequestID,
				Subcommand:     req.Subcommand,
				Status:         errorToStatus(err),
				PVResponseData: pvdata.NewPVAny(respData),
			}
			if err := c.SendApp(ctx, proto.APP_CHANNEL_RPC, resp); err != nil {
				ctxlog.L(ctx).Errorf("sending RPC response: %v", err)
			}

			c.mu.Lock()
			defer c.mu.Unlock()
			r.status = READY
			if req.Subcommand&proto.CHANNEL_RPC_DESTROY == proto.CHANNEL_RPC_DESTROY {
				r.status = DESTROYED
				delete(c.requests, req.RequestID)
			}
			return nil
		})
		return nil
	}
}

func (c *serverConn) handleRequestCancelDestroy(ctx context.Context, msg *connection.Message) error {
	var req proto.CancelDestroyRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if msg.Header.MessageCommand == proto.APP_REQUEST_DESTROY {
		ctxlog.L(ctx).Infof("REQUEST_DESTROY(%d, %d)", req.ServerChannelID, req.RequestID)
		if err := c.destroyRequestLocked(req.RequestID); err != nil {
			ctxlog.L(ctx).Errorf("destroying request %d: %v", req.RequestID, err)
		}
		return nil
	}

	ctxlog.L(ctx).Infof("REQUEST_CANCEL(%d, %d)", req.ServerChannelID, req.RequestID)
	if err := c.cancelRequestLocked(req.RequestID); err != nil {
		ctxlog.L(ctx).Errorf("cancelling request %d: %v", req.RequestID, err)
	}
	return nil
}

func (c *serverConn) handleSearchRequest(ctx context.Context, msg *connection.Message) error {
	var req proto.SearchRequest
	if err := msg.Decode(&req); err != nil {
		return err
	}
	ctxlog.L(ctx).Infof("received search request %#v", req)
	return c.srv.search.Search(ctx, c.Connection, req)
}
