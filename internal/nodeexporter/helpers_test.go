package nodeexporter

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeRuntimeServer is a minimal CRI RuntimeServiceServer + ImageServiceServer
// for testing. It returns a fixed list of containers and captures the last
// request for inspection. The pids map is keyed by container ID and used to
// populate the verbose ContainerStatus info JSON, mirroring what containerd /
// CRI-O return.
type fakeRuntimeServer struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer
	containers   []*runtimeapi.Container
	pids         map[string]int
	infoOverride map[string]string // raw info JSON keyed by container ID; takes precedence over pids
	// imageInfo is the raw verbose info JSON keyed by image ID, returned from
	// ImageStatus(verbose=true).Info["info"]. Set this to runtime-shaped JSON
	// (e.g. containerd's {"chainID":"...","imageSpec":{...}}) to exercise the
	// image-info parser.
	imageInfo map[string]string
	// images is the list returned by ListImages, used to exercise the
	// node-image collection path. Each entry's Id is also used by the
	// ImageStatus handler to look up tag/digest data; verbose info JSON for
	// that image still comes from the imageInfo map.
	images []*runtimeapi.Image
	// listImagesResult, when non-nil, overrides what ListImages returns.
	// Useful for tests that want ListImages to surface an image that
	// ImageStatus then can't find (i.e. a TOCTOU between the two RPCs).
	listImagesResult []*runtimeapi.Image
	// imageStatusErrors, when non-nil, maps an image ID to an error to
	// return from ImageStatus instead of a normal response.
	imageStatusErrors  map[string]error
	lastRequest        *runtimeapi.ListContainersRequest
	lastImageStatusReq *runtimeapi.ImageStatusRequest
	listImagesCalls    int
	imageStatusCalls   int
}

func (f *fakeRuntimeServer) ListContainers(_ context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	f.lastRequest = req
	return &runtimeapi.ListContainersResponse{Containers: f.containers}, nil
}

func (f *fakeRuntimeServer) ContainerStatus(_ context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	if !req.Verbose {
		return &runtimeapi.ContainerStatusResponse{}, nil
	}
	if raw, ok := f.infoOverride[req.ContainerId]; ok {
		return &runtimeapi.ContainerStatusResponse{
			Info: map[string]string{"info": raw},
		}, nil
	}
	if f.pids == nil {
		return &runtimeapi.ContainerStatusResponse{}, nil
	}
	pid, ok := f.pids[req.ContainerId]
	if !ok {
		return &runtimeapi.ContainerStatusResponse{}, nil
	}
	return &runtimeapi.ContainerStatusResponse{
		Info: map[string]string{
			"info": fmt.Sprintf(`{"pid":%d}`, pid),
		},
	}, nil
}

// ListImages returns the configured fake images. listImagesResult overrides
// images when set.
func (f *fakeRuntimeServer) ListImages(_ context.Context, _ *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	f.listImagesCalls++
	result := f.listImagesResult
	if result == nil {
		result = f.images
	}
	return &runtimeapi.ListImagesResponse{Images: result}, nil
}

// ImageStatus returns an Image (matched by Id from the configured images
// slice) plus the verbose info JSON looked up from imageInfo, both keyed by
// the request's image ID. Returns an empty response when no entry is set.
// If imageStatusErrors has an entry for the requested image ID, that error
// is returned instead.
func (f *fakeRuntimeServer) ImageStatus(_ context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	f.lastImageStatusReq = req
	f.imageStatusCalls++
	if req.Image == nil {
		return &runtimeapi.ImageStatusResponse{}, nil
	}
	if err, ok := f.imageStatusErrors[req.Image.Image]; ok {
		return nil, err
	}
	resp := &runtimeapi.ImageStatusResponse{}
	for _, img := range f.images {
		if img.Id == req.Image.Image {
			resp.Image = img
			break
		}
	}
	if req.Verbose {
		if raw, ok := f.imageInfo[req.Image.Image]; ok {
			resp.Info = map[string]string{"info": raw}
		}
	}
	return resp, nil
}

// startFakeRuntime starts a fake CRI gRPC server and returns a *grpc.ClientConn
// connected to it. The server is stopped automatically when t ends.
func startFakeRuntime(t *testing.T, srv *fakeRuntimeServer) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	s := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(s, srv)
	runtimeapi.RegisterImageServiceServer(s, srv)
	go s.Serve(lis) //nolint:errcheck
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// makeRootfs creates a fake procfs-style directory for the given pid under
// procRoot and writes the given os-release content into it.
func makeRootfs(t *testing.T, procRoot string, pid int, osReleaseContent string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "root", "etc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "os-release"), []byte(osReleaseContent), 0o644); err != nil {
		t.Fatalf("WriteFile os-release: %v", err)
	}
}

// findGatheredMetric returns the first *dto.Metric with the given name whose
// labels are a superset of labelFilter, or nil if none matches. A nil or empty
// labelFilter matches any metric with that name.
func findGatheredMetric(mfs []*dto.MetricFamily, name string, labelFilter map[string]string) *dto.Metric {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if gatheredLabelsMatch(m, labelFilter) {
				return m
			}
		}
	}
	return nil
}

func gatheredLabelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
