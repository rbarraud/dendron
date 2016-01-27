package main

import (
	"crypto/tls"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"login"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"proxy"
	"time"

	_ "github.com/lib/pq" /* Database driver for postgres */
)

var (
	synapseConfig  = flag.String("synapse-config", "homeserver.yaml", "Path to synapse's config")
	synapsePython  = flag.String("synapse-python", "python", "A python interpreter to use for synapse. This should be the python binary installed inside synapse's virtualenv. The interpreter will be looked up on the $PATH")
	synapseURL     = flag.String("synapse-url", "http://localhost:18448", "The HTTP URL that synapse is configured to listen on.")
	synapseDB      = flag.String("synapse-postgres", "", "Database config for the postgresql as per https://godoc.org/github.com/lib/pq#hdr-Connection_String_Parameters. This must point to the same database that synapse is configured to use")
	serverName     = flag.String("server-name", "", "Matrix server name. This must match the server_name configured for synapse.")
	macaroonSecret = flag.String("macaroon-secret", "", "Secret key for macaroons. This must match the macaroon_secret_key configured for synapse.")
	listenAddr     = flag.String("addr", ":8448", "Address to listen for matrix HTTPS requests on")
	listenCertFile = flag.String("cert-file", "", "TLS Certificate. This must match the tls_certificate_path configured for synapse.")
	listenKeyFile  = flag.String("key-file", "", "TLS Private Key. The private key for the certificate.")
)

func handleSignal(channel chan os.Signal, synapse *os.Process) {
	select {
	case sig := <-channel:
		log.Print("Got signal: ", sig)
		synapse.Signal(os.Interrupt)
		os.Exit(1)
	}
}

func waitForSynapse(sp *proxy.SynapseProxy) error {
	period := 10 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		log.Printf("Connecting to synapse...")
		if resp, err := sp.Client.Get(sp.URL.String()); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(period)
		period *= 2
	}
	return fmt.Errorf("failed to start synapse")
}

func main() {
	flag.Parse()

	db, err := sql.Open("postgres", *synapseDB)
	if err != nil {
		panic(err)
	}

	var synapseProxy proxy.SynapseProxy

	if u, err := url.Parse(*synapseURL); err != nil {
		panic(err)
	} else {
		synapseProxy.URL = *u
	}

	synapse := exec.Command(*synapsePython, "-m", "synapse.app.homeserver", "-c", *synapseConfig)
	synapse.Stderr = os.Stderr
	log.Print("Dendron: Starting synapse...")

	synapse.Start()

	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	go handleSignal(channel, synapse.Process)

	if err := waitForSynapse(&synapseProxy); err != nil {
		panic(err)
	}

	log.Print("Dendron: Synapse started")

	loginHandler, err := login.NewHandler(db, &synapseProxy, *serverName, *macaroonSecret)
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", &synapseProxy)
	mux.Handle("/_matrix/client/api/v1/login", loginHandler)
	mux.Handle("/_matrix/client/r0/login", loginHandler)
	mux.HandleFunc("/_dendron/test", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(w, "test")
	})

	cert, err := tls.LoadX509KeyPair(*listenCertFile, *listenKeyFile)
	if err != nil {
		panic(err)
	}

	c := tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	s := &http.Server{
		Addr:           *listenAddr,
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      &c,
	}

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		panic(err)
	}

	tlsListener := tls.NewListener(listener, s.TLSConfig)

	go s.Serve(tlsListener)

	if err := synapse.Wait(); err != nil {
		panic(err)
	}
}