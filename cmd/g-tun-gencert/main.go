// Command g-tun-gencert creates the self-signed certificate and private key
// used by the TLS based transports.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mahdi-1991/G-tun/internal/tlsutil"
)

func main() {
	certPath := flag.String("cert", "cert.pem", "where to write the certificate")
	keyPath := flag.String("key", "key.pem", "where to write the private key")
	flag.Parse()

	if err := tlsutil.GenerateSelfSigned(*certPath, *keyPath); err != nil {
		fmt.Fprintln(os.Stderr, "g-tun-gencert:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s and %s\n", *certPath, *keyPath)
}
