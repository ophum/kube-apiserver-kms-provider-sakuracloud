package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sacloud/sacloud-sdk-go/api/kms"
	v1 "github.com/sacloud/sacloud-sdk-go/api/kms/apis/v1"
	"github.com/sacloud/sacloud-sdk-go/common/saclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	v2 "k8s.io/kms/apis/v2"
)

const (
	annotationKey = "kms.sakuracloud.example.com"

	defaultSocketPath = "/run/kms-plugin/socket.sock"

	grpcShutdownTimeout       = 10 * time.Second
	keyVersionRefreshInterval = time.Minute
	keyVersionRefreshTimeout  = 30 * time.Second
)

type keyMeta struct {
	KeyID   string `json:"key_id"`
	Version int    `json:"version"`
	Algo    string `json:"algo"`
}

type kmsServer struct {
	v2.UnimplementedKeyManagementServiceServer

	keyOp kms.KeyAPI
	keyID string

	mu      sync.RWMutex
	version int
	keyRef  string
}

func newKMSServer() (*kmsServer, error) {
	var sa saclient.Client
	if err := sa.SetEnviron(os.Environ()); err != nil {
		return nil, fmt.Errorf("failed to load SAKURACLOUD_* environment variables: %w", err)
	}
	if err := sa.Populate(); err != nil {
		return nil, fmt.Errorf("failed to populate SAKURA Cloud API client: %w", err)
	}
	client, err := kms.NewClient(&sa)
	if err != nil {
		return nil, fmt.Errorf("failed to create SAKURA Cloud KMS client: %w", err)
	}

	keyID := os.Getenv("KMS_KEY_ID")
	if keyID == "" {
		return nil, fmt.Errorf("KMS_KEY_ID environment variable is required")
	}
	s := &kmsServer{
		keyOp: kms.NewKeyOp(client),
		keyID: keyID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyVersionRefreshTimeout)
	defer cancel()
	if err := s.refreshKeyVersion(ctx); err != nil {
		return nil, err
	}
	go s.refreshKeyVersionLoop()
	return s, nil
}

func (s *kmsServer) refreshKeyVersion(ctx context.Context) error {
	key, err := s.keyOp.Read(ctx, s.keyID)
	if err != nil {
		return fmt.Errorf("failed to read KMS key %s: %w", s.keyID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key.LatestVersion != s.version {
		slog.Info("KMS key version changed", "old", s.version, "new", key.LatestVersion)
	}
	s.version = key.LatestVersion
	s.keyRef = fmt.Sprintf("sakuracloud-kms:%s:v%d", s.keyID, s.version)
	return nil
}

func (s *kmsServer) refreshKeyVersionLoop() {
	ticker := time.NewTicker(keyVersionRefreshInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), keyVersionRefreshTimeout)
		if err := s.refreshKeyVersion(ctx); err != nil {
			slog.Error("failed to refresh KMS key version", "err", err)
		}
		cancel()
	}
}

func (s *kmsServer) currentKey() (keyRef string, version int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyRef, s.version
}

func (s *kmsServer) Status(ctx context.Context, _ *v2.StatusRequest) (*v2.StatusResponse, error) {
	keyRef, _ := s.currentKey()
	return &v2.StatusResponse{
		Version: "v2",
		Healthz: "ok",
		KeyId:   keyRef,
	}, nil
}

func (s *kmsServer) Encrypt(ctx context.Context, req *v2.EncryptRequest) (*v2.EncryptResponse, error) {
	if len(req.Plaintext) == 0 {
		return nil, status.Error(codes.InvalidArgument, "plaintext is empty")
	}
	slog.Info("encrypt", "uid", req.Uid, "bytes", len(req.Plaintext))

	keyRef, version := s.currentKey()
	cipher, err := s.keyOp.Encrypt(ctx, s.keyID, req.Plaintext, v1.EncryptionRequestAlgoAes256Gcm)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "SAKURA Cloud KMS encrypt failed: %v", err)
	}

	meta, err := json.Marshal(keyMeta{
		KeyID:   s.keyID,
		Version: version,
		Algo:    string(v1.EncryptionRequestAlgoAes256Gcm),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to encode key metadata: %v", err)
	}
	return &v2.EncryptResponse{
		Ciphertext: []byte(cipher),
		KeyId:      keyRef,
		Annotations: map[string][]byte{
			annotationKey: meta,
		},
	}, nil
}

func (s *kmsServer) Decrypt(ctx context.Context, req *v2.DecryptRequest) (*v2.DecryptResponse, error) {
	if len(req.Ciphertext) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ciphertext is empty")
	}
	slog.Info("decrypt", "uid", req.Uid, "key_id", req.KeyId)

	keyID := s.keyID
	if v, ok := req.Annotations[annotationKey]; ok {
		var meta keyMeta
		if err := json.Unmarshal(v, &meta); err == nil && meta.KeyID != "" {
			keyID = meta.KeyID
		}
	}

	plain, err := s.keyOp.Decrypt(ctx, keyID, string(req.Ciphertext))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "SAKURA Cloud KMS decrypt failed: %v", err)
	}
	return &v2.DecryptResponse{Plaintext: plain}, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	socketPath := os.Getenv("KMS_SOCKET")
	if socketPath == "" {
		socketPath = defaultSocketPath
	}
	if err := run(socketPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(socketPath string) error {
	srv, err := newKMSServer()
	if err != nil {
		return err
	}

	if dir := filepath.Dir(socketPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create socket directory %s: %w", dir, err)
		}
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale socket %s: %w", socketPath, err)
	}
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o777); err != nil {
		return fmt.Errorf("failed to chmod socket %s: %w", socketPath, err)
	}

	gs := grpc.NewServer()
	v2.RegisterKeyManagementServiceServer(gs, srv)
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, healthSrv)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		keyRef, _ := srv.currentKey()
		slog.Info("KMS v2 plugin listening", "socket", "unix://"+socketPath, "key", keyRef)
		serveErr <- gs.Serve(lis)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("gRPC server failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutting down")
		stopped := make(chan struct{})
		go func() {
			gs.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(grpcShutdownTimeout):
			gs.Stop()
		}
		_ = os.Remove(socketPath)
		return nil
	}
}
