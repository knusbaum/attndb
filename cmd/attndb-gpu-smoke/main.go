//go:build onnx

// attndb-gpu-smoke tests the native Go/ORT path without a vault, Qdrant, or MCP.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/kjn/attndb/internal/encode/onnx"
	ort "github.com/yalue/onnxruntime_go"
)

func main() {
	model := flag.String("model", "/opt/attndb/models", "exported model directory")
	devicesOnly := flag.Bool("devices-only", false, "register the plugin and require a MIGraphX device, without loading models")
	flag.Parse()
	if err := run(*model, *devicesOnly); err != nil {
		fmt.Fprintln(os.Stderr, "GPU smoke FAILED:", err)
		os.Exit(1)
	}
}

func enumerate() error {
	devices, err := ort.GetEpDevices()
	if err != nil {
		return err
	}
	found := false
	for _, d := range devices {
		fmt.Printf("device ep=%s vendor=%s\n", d.EpName(), d.EpVendor())
		found = found || d.EpName() == "MIGraphXExecutionProvider"
	}
	if !found {
		return fmt.Errorf("no MIGraphX device")
	}
	return nil
}

func run(model string, devicesOnly bool) error {
	for _, key := range []string{"ATTNDB_ORT_LIB", "ATTNDB_MIGRAPHX_EP_LIB"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("%s must be set", key)
		}
	}
	if devicesOnly {
		ort.SetSharedLibraryPath(os.Getenv("ATTNDB_ORT_LIB"))
		if err := ort.InitializeEnvironment(); err != nil {
			return err
		}
		defer ort.DestroyEnvironment()
		fmt.Printf("ORT %s\n", ort.GetVersion())
		if err := ort.RegisterExecutionProviderLibrary("MIGraphXExecutionProvider", os.Getenv("ATTNDB_MIGRAPHX_EP_LIB")); err != nil {
			return err
		}
		return enumerate()
	}
	cache := os.Getenv("ATTNDB_MIGRAPHX_CACHE_DIR")
	if cache == "" {
		return fmt.Errorf("ATTNDB_MIGRAPHX_CACHE_DIR must be set to a disposable smoke cache")
	}
	start := time.Now()
	// These production constructors enforce session.disable_cpu_ep_fallback=1,
	// append the enumerated plugin device, and warm their supported shapes.
	cb, err := onnx.NewColBERT(model, "migraphx")
	if err != nil {
		return err
	}
	defer cb.Close()
	if err := enumerate(); err != nil {
		return err
	}
	sv, err := onnx.NewSingleVector(filepath.Join(model, "single"), "migraphx", false)
	if err != nil {
		return err
	}
	defer sv.Close()
	ctx := context.Background()
	const text = "The GPU smoke test checks document and query embeddings."
	cq, err := cb.EncodeQuery(ctx, text)
	if err != nil {
		return err
	}
	cd, err := cb.EncodeDocs(ctx, []string{text})
	if err != nil {
		return err
	}
	sq, err := sv.EncodeQuery(ctx, text)
	if err != nil {
		return err
	}
	sd, err := sv.EncodeDocs(ctx, []string{text})
	if err != nil {
		return err
	}
	if len(cd) != 1 || len(sd) != 1 || len(cq.Vecs) == 0 || len(cd[0].Vecs) == 0 {
		return fmt.Errorf("empty or incorrect document/token batch")
	}
	for _, rows := range [][][]float32{cq.Vecs, cd[0].Vecs} {
		for _, row := range rows {
			if err := vector(row, cb.Dim()); err != nil {
				return err
			}
		}
	}
	for _, row := range [][]float32{sq, sd[0]} {
		if err := vector(row, sv.Dim()); err != nil {
			return err
		}
	}
	// Each model fingerprint must have nonempty serialized cache output.
	entries, err := os.ReadDir(cache)
	if err != nil {
		return err
	}
	populated := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files := 0
		err := filepath.WalkDir(filepath.Join(cache, entry.Name()), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				if info.Size() > 0 {
					files++
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if files > 0 {
			populated++
		}
	}
	if populated < 2 {
		return fmt.Errorf("expected nonempty caches for both models, found %d", populated)
	}
	fmt.Printf("PASS: ORT %s, both models query+document inference, finite nonzero vectors, CPU EP fallback disabled, %d model caches, elapsed %s\n",
		ort.GetVersion(), populated, time.Since(start).Round(time.Millisecond))
	return nil
}

func vector(v []float32, dim int) error {
	if len(v) != dim {
		return fmt.Errorf("vector dimension %d, expected %d", len(v), dim)
	}
	var norm float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return fmt.Errorf("nonfinite embedding")
		}
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return fmt.Errorf("zero embedding")
	}
	return nil
}
