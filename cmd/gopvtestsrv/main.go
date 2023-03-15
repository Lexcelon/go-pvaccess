package main

import (
	"context"
	"flag"

	"fmt"
	"os"
	"os/signal"

	// "reflect"
	"syscall"

	// "time"

	// "time"

	log "github.com/sirupsen/logrus"

	pvaccess "github.com/Lexcelon/go-pvaccess"
	"github.com/Lexcelon/go-pvaccess/internal/ctxlog"
	"github.com/Lexcelon/go-pvaccess/pvdata"
)

var (
	disableSearch = flag.Bool("disable_search", false, "disable UDP beacon/search support")
	verbose       = flag.Bool("v", false, "verbose mode")
)

type ChannelValues struct {
	v interface{}
}

// putter implements the ChannelPuter interface to set the channel value
type putter struct {
	c *pvaccess.SimpleChannel
}

func (p *putter) ChannelPut(value interface{}, ctx context.Context) (interface{}, error) {
	// Set the channel value to the requested value
	p.c.Set(&ChannelValues{v: value})
	return nil, nil
}

func main() {
	flag.Parse()

	log.SetLevel(log.DebugLevel)
	if *verbose {
		log.SetLevel(log.TraceLevel)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		ctxlog.L(ctx).Infof("received signal %s; exiting", sig)
		cancel()
	}()
	s, err := pvaccess.NewServer()
	if err != nil {
		ctxlog.L(ctx).Fatalf("creating server: %v", err)
	}
	s.DisableSearch = *disableSearch

	// var myInt uint16 = 23
	// pMyInt := pvdata.PVUShort(myInt)
	// cget := pvaccess.NewSimpleChannel("gopvgettest")
	// cget.Set(&pMyInt)
	// returned := cget.Get()
	// log.Infof("returned: %v", returned)
	// s.AddChannelProvider(cget)
	// myShort := pvdata.PVShort(42)

	// shortField := pvdata.PVScalar(pvdata.Uint16)

	// initValue, err := pvdata.NewPVStructure(map[string]pvdata.PVField{
	// 	"value": shortField,
	// })
	initValue, err := pvdata.NewPVStructure(&ChannelValues{v: pvdata.PVUShort(23)})
	if err != nil {
		panic(err)
	}
	// fmt.Println("puttable value", reflect.ValueOf(initValue.ID))
	cput := pvaccess.NewSimpleChannel("test", initValue)
	puttablevalue := pvdata.PVUShort(23)

	fmt.Println("setting puttable value", puttablevalue)
	cput.Set(&puttablevalue)
	// fmt.Println("finished setting puttable value")

	// cput.PutCreator = func(ctx context.Context, req pvdata.PVField) (pvaccess.ChannelPuter, error) {
	// 	// Implement the ChannelPuter interface to set the channel value
	// 	return &putter{c: cput}, nil
	// }

	s.AddChannelProvider(cput)

	// thisInitVal, err := pvdata.NewPVStructure(&ChannelValues{Value: pvdata.PVULong(256)})
	// c := pvaccess.NewSimpleChannel("gopvtest", thisInitVal)
	// value := pvdata.PVLong(256)
	// c.Set(&value)
	// s.AddChannelProvider(c)
	// go func() {
	// 	for range time.Tick(time.Second) {
	// 		value++
	// 		c.Set(&value)
	// 	}
	// }()

	s.ListenAndServe(ctx)
}
