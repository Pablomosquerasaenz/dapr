package security

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"sync"
	"time"

	grpcMiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcRetry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	daprCredentials "github.com/dapr/dapr/pkg/credentials"
	diag "github.com/dapr/dapr/pkg/diagnostics"
	sentryv1pb "github.com/dapr/dapr/pkg/proto/sentry/v1"
)

const (
	TLSServerName     = "cluster.local"
	sentrySignTimeout = time.Second * 5
	certType          = "CERTIFICATE"
	kubeTknPath       = "/var/run/secrets/dapr.io/sentrytoken/token"
	sentryMaxRetries  = 100
)

type Authenticator interface {
	GetTrustAnchors() *x509.CertPool
	GetCurrentSignedCert() *SignedCertificate
	CreateSignedWorkloadCert(id, namespace, trustDomain string) (*SignedCertificate, error)
}

type authenticator struct {
	trustAnchors      *x509.CertPool
	certChainPem      []byte
	keyPem            []byte
	genCSRFunc        func(id string) ([]byte, []byte, error)
	sentryAddress     string
	currentSignedCert *SignedCertificate
	certMutex         *sync.RWMutex
}

type SignedCertificate struct {
	WorkloadCert  []byte
	PrivateKeyPem []byte
	Expiry        time.Time
	TrustChain    *x509.CertPool
}

func newAuthenticator(sentryAddress string, trustAnchors *x509.CertPool, certChainPem, keyPem []byte, genCSRFunc func(id string) ([]byte, []byte, error)) Authenticator {
	return &authenticator{
		trustAnchors:  trustAnchors,
		certChainPem:  certChainPem,
		keyPem:        keyPem,
		genCSRFunc:    genCSRFunc,
		sentryAddress: sentryAddress,
		certMutex:     &sync.RWMutex{},
	}
}

// GetTrustAnchors returns the extracted root cert that serves as the trust anchor.
func (a *authenticator) GetTrustAnchors() *x509.CertPool {
	return a.trustAnchors
}

// GetCurrentSignedCert returns the current and latest signed certificate.
func (a *authenticator) GetCurrentSignedCert() *SignedCertificate {
	a.certMutex.RLock()
	defer a.certMutex.RUnlock()
	return a.currentSignedCert
}

// CreateSignedWorkloadCert returns a signed workload certificate, the PEM encoded private key
// And the duration of the signed cert.
func (a *authenticator) CreateSignedWorkloadCert(id, namespace, trustDomain string) (*SignedCertificate, error) {
	csrb, pkPem, err := a.genCSRFunc(id)
	if err != nil {
		return nil, err
	}
	certPem := pem.EncodeToMemory(&pem.Block{Type: certType, Bytes: csrb})

	config, err := daprCredentials.TLSConfigFromCertAndKey(a.certChainPem, a.keyPem, TLSServerName, a.trustAnchors)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create tls config from cert and key")
	}

	unaryClientInterceptor := grpcRetry.UnaryClientInterceptor()

	if diag.DefaultGRPCMonitoring.IsEnabled() {
		unaryClientInterceptor = grpcMiddleware.ChainUnaryClient(
			unaryClientInterceptor,
			diag.DefaultGRPCMonitoring.UnaryClientInterceptor(),
		)
	}

	conn, err := grpc.Dial(
		a.sentryAddress,
		grpc.WithTransportCredentials(credentials.NewTLS(config)),
		grpc.WithUnaryInterceptor(unaryClientInterceptor))
	if err != nil {
		diag.DefaultMonitoring.MTLSWorkLoadCertRotationFailed("sentry_conn")
		return nil, errors.Wrap(err, "error establishing connection to sentry")
	}
	defer conn.Close()

	c := sentryv1pb.NewCAClient(conn)

	resp, err := c.SignCertificate(context.Background(),
		&sentryv1pb.SignCertificateRequest{
			CertificateSigningRequest: certPem,
			Id:                        getSentryIdentifier(id),
			Token:                     getToken(),
			TrustDomain:               trustDomain,
			Namespace:                 namespace,
		}, grpcRetry.WithMax(sentryMaxRetries), grpcRetry.WithPerRetryTimeout(sentrySignTimeout))
	if err != nil {
		diag.DefaultMonitoring.MTLSWorkLoadCertRotationFailed("sign")
		return nil, errors.Wrap(err, "error from sentry SignCertificate")
	}

	workloadCert := resp.GetWorkloadCertificate()
	validTimestamp := resp.GetValidUntil()
	if err = validTimestamp.CheckValid(); err != nil {
		diag.DefaultMonitoring.MTLSWorkLoadCertRotationFailed("invalid_ts")
		return nil, errors.Wrap(err, "error parsing ValidUntil")
	}

	expiry := validTimestamp.AsTime()
	trustChain := x509.NewCertPool()
	for _, c := range resp.GetTrustChainCertificates() {
		ok := trustChain.AppendCertsFromPEM(c)
		if !ok {
			diag.DefaultMonitoring.MTLSWorkLoadCertRotationFailed("chaining")
			return nil, errors.Wrap(err, "failed adding trust chain cert to x509 CertPool")
		}
	}

	signedCert := &SignedCertificate{
		WorkloadCert:  workloadCert,
		PrivateKeyPem: pkPem,
		Expiry:        expiry,
		TrustChain:    trustChain,
	}

	a.certMutex.Lock()
	defer a.certMutex.Unlock()

	a.currentSignedCert = signedCert
	return signedCert, nil
}

// currently we support Kubernetes identities.
func getToken() string {
	b, _ := os.ReadFile(kubeTknPath)
	return string(b)
}

func getSentryIdentifier(appID string) string {
	// return injected identity, default id if not present
	localID := os.Getenv("SENTRY_LOCAL_IDENTITY")
	if localID != "" {
		return localID
	}
	return appID
}
