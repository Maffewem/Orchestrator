package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// dnsCertManager gets and renews a Let's Encrypt certificate for the dashboard using the
// DNS-01 challenge through Cloudflare. Unlike the TLS-ALPN challenge it needs no inbound
// connection from the internet, so it works for LAN-only names that resolve to a private
// address.
type dnsCertManager struct {
	host, dir, token, directory string
	srv                         *Server

	mu      sync.Mutex
	cert    *tls.Certificate
	leaf    *x509.Certificate
	working bool
	lastErr string
}

func newDNSCertManager(s *Server, host, dir, token string) *dnsCertManager {
	m := &dnsCertManager{srv: s, host: host, dir: dir, token: token, directory: acme.LetsEncryptURL}
	m.loadCached()
	return m
}

func (m *dnsCertManager) loadCached() {
	b, err := os.ReadFile(filepath.Join(m.dir, m.host+".dns.pem"))
	if err != nil {
		return
	}
	pair, err := tls.X509KeyPair(b, b)
	if err != nil {
		return
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return
	}
	if leaf, err := x509.ParseCertificate(block.Bytes); err == nil && time.Now().Before(leaf.NotAfter) {
		m.cert, m.leaf = &pair, leaf
	}
}

func (m *dnsCertManager) status() (*x509.Certificate, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaf, m.lastErr
}

func (m *dnsCertManager) needsRenewal() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaf == nil || time.Until(m.leaf.NotAfter) < 30*24*time.Hour
}

// run obtains the certificate when missing and checks for renewal twice a day until ctx ends.
func (m *dnsCertManager) run(ctx context.Context) {
	for {
		if m.needsRenewal() {
			err := m.obtain(ctx)
			m.mu.Lock()
			m.lastErr = ""
			if err != nil {
				m.lastErr = err.Error()
				m.srv.log.Error("dashboard certificate (DNS challenge)", "err", err)
			}
			m.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(12 * time.Hour):
		}
	}
}

func (m *dnsCertManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cert == nil {
		if m.lastErr != "" {
			return nil, errors.New("no certificate yet: " + m.lastErr)
		}
		return nil, errors.New("the certificate is still being requested")
	}
	return m.cert, nil
}

func (m *dnsCertManager) accountKey() (*ecdsa.PrivateKey, error) {
	path := filepath.Join(m.dir, "dns-account.key")
	if b, err := os.ReadFile(path); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalECPrivateKey(k)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func (m *dnsCertManager) obtain(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	key, err := m.accountKey()
	if err != nil {
		return err
	}
	cl := &acme.Client{Key: key, DirectoryURL: m.directory}
	if _, err := cl.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("registering with Let's Encrypt: %w", err)
	}
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(m.host))
	if err != nil {
		return fmt.Errorf("creating the order: %w", err)
	}
	zone, err := m.srv.cfZoneFor(ctx, m.token, m.host)
	if err != nil {
		return err
	}
	if zone == nil {
		return fmt.Errorf("no Cloudflare zone found for %s: check the token can read that zone", m.host)
	}
	for _, u := range order.AuthzURLs {
		if err := m.authorize(ctx, cl, zone.ID, u); err != nil {
			return err
		}
	}
	if order, err = cl.WaitOrder(ctx, order.URI); err != nil {
		return fmt.Errorf("waiting for the order: %w", err)
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{m.host}}, certKey)
	if err != nil {
		return err
	}
	der, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("finalizing the order: %w", err)
	}
	var out []byte
	for _, d := range der {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	kb, _ := x509.MarshalECPrivateKey(certKey)
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})...)
	pair, err := tls.X509KeyPair(out, out)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.dir, m.host+".dns.pem"), out, 0o600); err != nil {
		m.srv.log.Error("saving dashboard certificate", "err", err)
	}
	m.mu.Lock()
	m.cert, m.leaf = &pair, leaf
	m.mu.Unlock()
	m.srv.event(ctx, "", "info", "settings.https", "Obtained a Let's Encrypt certificate for "+m.host+" (DNS challenge)")
	return nil
}

func (m *dnsCertManager) authorize(ctx context.Context, cl *acme.Client, zoneID, authzURL string) error {
	az, err := cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return err
	}
	if az.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range az.Challenges {
		if c.Type == "dns-01" {
			chal = c
		}
	}
	if chal == nil {
		return errors.New("Let's Encrypt offered no DNS challenge")
	}
	val, err := cl.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return err
	}
	name := "_acme-challenge." + m.host
	var rec struct {
		ID string `json:"id"`
	}
	body := map[string]any{"type": "TXT", "name": name, "content": val, "ttl": 60}
	if err := m.srv.cfCall(ctx, m.token, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, &rec); err != nil {
		return fmt.Errorf("adding the challenge record (the token needs Zone > DNS > Edit): %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m.srv.cfCall(dctx, m.token, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+rec.ID, nil, nil)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second): // let Cloudflare publish the record
	}
	if _, err := cl.Accept(ctx, chal); err != nil {
		return err
	}
	if _, err := cl.WaitAuthorization(ctx, az.URI); err != nil {
		return fmt.Errorf("Let's Encrypt could not verify the DNS record: %w", err)
	}
	return nil
}
