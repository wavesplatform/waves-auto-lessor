// +build darwin dragonfly freebsd linux netbsd openbsd solaris

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func init() {
	interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func interruptListener() context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, interruptSignals...)
		sig := <-signals
		log.Printf("Caught signal '%s', aborting...", sig)
		cancel()
		for sig := range signals {
			log.Printf("Caught signal '%s' again, already in progress", sig)
		}
	}()
	return ctx
}
