# Objective

Handle put request properly. When using the command `pvput [channel name] [value]` the server should be able to handle the request and update the value of the channel.

# Cause of failure

`msg.Decode` in `server.go:493` is unable to decode the request into a put request struct.

# Details

The incoming request of type `Message` is fails to be decoded into a `ChannelPutRequest` struct, but the `Data` field of the `Message` appears to be parsed incorrectly from the message sent over the wire. It appears to always be `[65 48 32 16 82 96 112 128 8 254 1 0]` in my case




# Types info

```go
type Message struct {
    Header proto.PVAccessHeader
    Data   []byte

    c      *Connection
    reader pvdata.Reader
}

type ChannelPutRequest struct {
    ServerChannelID pvdata.PVInt
    RequestID       pvdata.PVInt
    Subcommand      pvdata.PVByte
    // For put_init:
    PVRequest pvdata.PVField
    // For put:
    ToPutBitSet        pvdata.PVBitSet
    PVPutStructureData pvdata.PVField
}
```