package easyraft

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeNodeCerts issues a CA and one node certificate under it, writes all
// three PEM files into dir, and returns their paths.
func writeNodeCerts(t *testing.T, dir, nodeID string) (certFile, keyFile, caFile string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "easyraft test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nodeTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth,
		},
		DNSNames: []string{nodeID, "localhost"},
	}
	nodeDER, err := x509.CreateCertificate(rand.Reader, nodeTmpl, caCert, &nodeKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	nodeKeyDER, err := x509.MarshalECPrivateKey(nodeKey)
	if err != nil {
		t.Fatal(err)
	}

	certFile = filepath.Join(dir, "node.crt")
	keyFile = filepath.Join(dir, "node.key")
	caFile = filepath.Join(dir, "ca.crt")
	write := func(path, blockType string, der []byte) {
		t.Helper()
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(certFile, "CERTIFICATE", nodeDER)
	write(keyFile, "EC PRIVATE KEY", nodeKeyDER)
	write(caFile, "CERTIFICATE", caDER)
	return certFile, keyFile, caFile
}

// TestResolveTLSFiles_BuildsWhatARaftMeshNeeds pins the configuration the
// option exists to get right: both ends prove who they are, the authority is
// trusted in both directions, and a peer authorizer follows from that.
func TestResolveTLSFiles_BuildsWhatARaftMeshNeeds(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := writeNodeCerts(t, dir, "n1")

	var c config
	WithTLSFiles(certFile, keyFile, caFile)(&c)
	if err := resolveTLSFiles(&c); err != nil {
		t.Fatalf("resolveTLSFiles: %v", err)
	}
	if c.TLS == nil {
		t.Fatal("no TLS configuration was built")
	}
	if got := c.TLS.ClientAuth; got != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert: anything weaker leaves "+
			"the Raft port open to any client that can reach it", got)
	}
	if c.TLS.RootCAs == nil || c.TLS.ClientCAs == nil {
		t.Error("the authority must be trusted in both directions: every node is both a client and a server")
	}
	if len(c.TLS.Certificates) != 1 {
		t.Errorf("got %d certificates, want this node's one", len(c.TLS.Certificates))
	}
	if c.TLS.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x", c.TLS.MinVersion)
	}
	// The whole point of getting ClientAuth right: the peer authorizer applies.
	if peerAuthorizerFor(&c) == nil {
		t.Error("no peer authorizer was installed for a mutual-TLS configuration")
	}
}

// TestResolveTLSFiles_ReportsWhatIsWrong pins that a bad configuration fails
// where the caller is, rather than at the first connection between two nodes.
func TestResolveTLSFiles_ReportsWhatIsWrong(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := writeNodeCerts(t, dir, "n1")

	cases := map[string]struct {
		opts []Option
		want string
	}{
		"missing the authority": {
			opts: []Option{WithTLSFiles(certFile, keyFile, "")},
			want: "all three",
		},
		"a path that is not there": {
			opts: []Option{WithTLSFiles(filepath.Join(dir, "nope.crt"), keyFile, caFile)},
			want: "load Raft TLS certificate",
		},
		"an authority that is not one": {
			opts: []Option{WithTLSFiles(certFile, keyFile, keyFile)},
			want: "no certificate a pool would accept",
		},
		"both ways of saying it": {
			opts: []Option{
				WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
				WithTLSFiles(certFile, keyFile, caFile),
			},
			want: "use one or the other",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c config
			for _, o := range tc.opts {
				o(&c)
			}
			err := resolveTLSFiles(&c)
			if err == nil {
				t.Fatal("accepted a configuration that cannot work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// And nothing at all stays nothing at all.
	var empty config
	if err := resolveTLSFiles(&empty); err != nil || empty.TLS != nil {
		t.Errorf("a configuration with no TLS files produced (%v, %v)", empty.TLS, err)
	}
}
