package store

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/kjn/attndb/internal/core"
	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Use the real client against an in-process gRPC service: no live Qdrant,
// network port, collection writes, or model runtimes are needed.
type queryServer struct {
	mu          sync.Mutex
	exists      bool
	queryErr    error
	existsErr   error
	existsCalls int
	names       []string
}

type queryPointsServer struct {
	qdrant.UnimplementedPointsServer
	*queryServer
}

type queryCollectionsServer struct {
	qdrant.UnimplementedCollectionsServer
	*queryServer
}

func (s *queryPointsServer) Query(_ context.Context, req *qdrant.QueryPoints) (*qdrant.QueryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, req.CollectionName)
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	return &qdrant.QueryResponse{Result: []*qdrant.ScoredPoint{{
		Score:   0.9,
		Payload: qdrant.NewValueMap(map[string]any{"doc_id": "later-ingest", "span_start": 0, "span_end": 12}),
	}}}, nil
}

func (s *queryCollectionsServer) CollectionExists(_ context.Context, req *qdrant.CollectionExistsRequest) (*qdrant.CollectionExistsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, req.CollectionName)
	s.existsCalls++
	if s.existsErr != nil {
		return nil, s.existsErr
	}
	return &qdrant.CollectionExistsResponse{Result: &qdrant.CollectionExists{Exists: s.exists}}, nil
}

func newQueryTestPool(t *testing.T, s *queryServer) *Qdrant {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	qdrant.RegisterPointsServer(server, &queryPointsServer{queryServer: s})
	qdrant.RegisterCollectionsServer(server, &queryCollectionsServer{queryServer: s})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	client, err := qdrant.NewClient(&qdrant.Config{
		Host: "127.0.0.1", PoolSize: 1, SkipCompatibilityCheck: true,
		GrpcOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &Qdrant{client: client, name: "attndb_test_tok_section_onnx"}
}

func queryKinds() map[string]func(*Qdrant, context.Context) ([]Scored, error) {
	return map[string]func(*Qdrant, context.Context) ([]Scored, error){
		"single": func(q *Qdrant, ctx context.Context) ([]Scored, error) {
			return q.SearchSingle(ctx, []float32{1, 0}, 10, nil)
		},
		"multi": func(q *Qdrant, ctx context.Context) ([]Scored, error) {
			return q.SearchMulti(ctx, core.TokenVecs{Vecs: [][]float32{{1, 0}, {0, 1}}}, 10, nil)
		},
	}
}

func TestQdrantQueryFreshStore(t *testing.T) {
	for kind, search := range queryKinds() {
		t.Run(kind, func(t *testing.T) {
			s := &queryServer{queryErr: status.Error(codes.NotFound, "Collection does not exist")}
			q := newQueryTestPool(t, s)
			ctx := t.Context()
			// Startup reconcile sees an empty index and an empty vault never
			// upserts anything, leaving the collection absent.
			if stamps, err := q.Stamps(ctx); err != nil || len(stamps) != 0 {
				t.Fatalf("fresh stamps = %v, %v", stamps, err)
			}
			for i := 0; i < 2; i++ {
				if hits, err := search(q, ctx); err != nil || len(hits) != 0 {
					t.Fatalf("fresh search = %v, %v", hits, err)
				}
			}
			// Simulate a later ingest (possibly by another client). An earlier
			// empty search must not cache absence or prevent subsequent hits.
			s.mu.Lock()
			s.exists, s.queryErr = true, nil
			s.mu.Unlock()
			hits, err := search(q, ctx)
			if err != nil || len(hits) != 1 || hits[0].Rec.DocID != "later-ingest" {
				t.Fatalf("search after ingest = %v, %v", hits, err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.existsCalls != 3 {
				t.Fatalf("existence calls = %d; want startup + two absent queries, none on success", s.existsCalls)
			}
			for _, name := range s.names {
				if name != q.name {
					t.Fatalf("collection namespace changed: %q", name)
				}
			}
		})
	}
}

func TestQdrantQueryErrors(t *testing.T) {
	for kind, search := range queryKinds() {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name            string
				queryCode       codes.Code
				exists          bool
				existsCode      codes.Code
				wantCode        codes.Code
				wantExistsCalls int
			}{
				{"other missing resource", codes.NotFound, true, codes.OK, codes.NotFound, 1},
				{"dimension mismatch", codes.InvalidArgument, false, codes.OK, codes.InvalidArgument, 0},
				{"unavailable", codes.Unavailable, false, codes.OK, codes.Unavailable, 0},
				{"permission denied", codes.PermissionDenied, false, codes.OK, codes.PermissionDenied, 0},
				{"unauthenticated", codes.Unauthenticated, false, codes.OK, codes.Unauthenticated, 0},
				{"deadline", codes.DeadlineExceeded, false, codes.OK, codes.DeadlineExceeded, 0},
				{"internal", codes.Internal, false, codes.OK, codes.Internal, 0},
				{"existence check failed", codes.NotFound, false, codes.Unavailable, codes.Unavailable, 1},
				{"existence check not found", codes.NotFound, false, codes.NotFound, codes.NotFound, 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := &queryServer{
						queryErr: status.Error(tc.queryCode, "query failure"),
						exists:   tc.exists, existsErr: status.Error(tc.existsCode, "existence failure"),
					}
					q := newQueryTestPool(t, s)
					hits, err := search(q, t.Context())
					if status.Code(err) != tc.wantCode || len(hits) != 0 {
						t.Fatalf("got %v, %v; want error %s", hits, err, tc.wantCode)
					}
					if !strings.Contains(err.Error(), q.name) {
						t.Fatalf("missing collection context: %v", err)
					}
					s.mu.Lock()
					defer s.mu.Unlock()
					if s.existsCalls != tc.wantExistsCalls {
						t.Fatalf("existence calls = %d, want %d", s.existsCalls, tc.wantExistsCalls)
					}
				})
			}
		})
	}
}
