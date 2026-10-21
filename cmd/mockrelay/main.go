// Command mockrelay is the fake OpenAI-compatible upstream of the test
// environment (design §6.6). It listens on the docker0 gateway so containers can
// reach it, and it is never part of the production path.
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	listen := flag.String("listen", "172.17.0.1:18080", "address to listen on")
	flag.Parse()

	log.Printf("mockrelay listening on %s", *listen)
	if err := http.ListenAndServe(*listen, newHandler()); err != nil {
		log.Fatal(err)
	}
}
