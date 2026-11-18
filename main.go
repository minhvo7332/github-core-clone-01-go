// The forge server: bind an ephemeral port, print it, serve forever.
//
// The Node launcher (main.ts) starts this binary, reads the PORT line, and
// registers the port with the env. Run directly, it is also the process the
// local bench harness drives: listen on 127.0.0.1:0 and print `PORT <n>`.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("PORT %d\n", port)
	os.Stdout.Sync()

	a := newApp()
	go serve(ln, a.dispatch)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ln.Close()
}
