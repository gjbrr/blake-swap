// Command blake-swap-relay is a local Nostr relay for testing and two-host runs.
//
// A swap between two implementations needs one shared thing: a relay. Using a public one for
// testing makes the result depend on somebody else's retention policy and rate limits, and
// makes a two-host regtest walkthrough need the internet for no protocol reason. This serves
// the market's subscriptions locally instead.
//
// It is a test utility, not a relay: events live in memory for the life of the process, there
// is no authentication and no persistence, and it does not verify signatures — every client in
// this protocol verifies what it reads, which is where the obligation belongs.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7777", "address to listen on")
	flag.Parse()

	relay, err := relaystub.Listen(*addr)

	if err != nil {
		fmt.Fprintf(os.Stderr, "blake-swap-relay: %v\n", err)

		os.Exit(1)
	}

	fmt.Printf("NOSTR RELAY\n===========\nurl: %s\n", relay.URL)
	fmt.Println("holding events in memory for the life of this process")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	<-stop

	relay.Close()
}
