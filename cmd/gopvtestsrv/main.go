package main

import (
	"context"
	"flag"
	"time"

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

	cput := pvaccess.NewSimpleChannel("test")
	puttablevalue := pvdata.PVFloat(1.1)

	fmt.Println("setting puttable value", puttablevalue)
	cput.Set(&puttablevalue)

	s.AddChannelProvider(cput)

	c := pvaccess.NewSimpleChannel("gopvtest")
	value := pvdata.PVLong(256)
	c.Set(&value)
	s.AddChannelProvider(c)
	go func() {
		for range time.Tick(time.Second) {
			value++
			c.Set(&value)
		}
	}()

	s.ListenAndServe(ctx)
}
