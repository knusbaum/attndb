//go:build onnx

package onnx

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const migraphxEP = "MIGraphXExecutionProvider"

var loadMIGraphX = sync.OnceValues(func() (ort.EpDevice, error) {
	path := os.Getenv("ATTNDB_MIGRAPHX_EP_LIB")
	if path == "" {
		return ort.EpDevice{}, fmt.Errorf("set ATTNDB_MIGRAPHX_EP_LIB to the AMD MIGraphX execution provider plugin library")
	}
	if err := ort.RegisterExecutionProviderLibrary(migraphxEP, path); err != nil {
		return ort.EpDevice{}, fmt.Errorf("register MIGraphX plugin %q: %w", path, err)
	}
	devices, err := ort.GetEpDevices()
	if err != nil {
		return ort.EpDevice{}, fmt.Errorf("enumerate MIGraphX devices: %w", err)
	}
	for _, device := range devices {
		if device.EpName() == migraphxEP {
			return device, nil
		}
	}
	return ort.EpDevice{}, fmt.Errorf("MIGraphX plugin reported no available device")
})

func appendMIGraphX(opts *ort.SessionOptions, modelPath string) error {
	device, err := loadMIGraphX()
	if err != nil {
		return err
	}
	if err := opts.AddSessionConfigEntry("session.disable_cpu_ep_fallback", "1"); err != nil {
		return fmt.Errorf("disable CPU fallback: %w", err)
	}
	options := map[string]string{
		"fp16_enable":           "0",
		"bf16_enable":           "0",
		"int8_enable":           "0",
		"fp8_enable":            "0",
		"coresident_programs":   "1",
		"max_resident_programs": "4",
	}
	if root := os.Getenv("ATTNDB_MIGRAPHX_CACHE_DIR"); root != "" {
		cache, err := migraphxCachePath(root, modelPath)
		if err != nil {
			return fmt.Errorf("fingerprint MIGraphX model: %w", err)
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			return fmt.Errorf("create MIGraphX cache directory: %w", err)
		}
		options["cache_dir"] = cache
	}
	if err := opts.AppendExecutionProviderV2([]ort.EpDevice{device}, options); err != nil {
		return fmt.Errorf("append MIGraphX provider: %w", err)
	}
	return nil
}

// The provider's graph key doesn't include initializer contents. Scope its
// cache to the ONNX export and the external weights emitted by our exporters.
func migraphxCachePath(root, modelPath string) (string, error) {
	var hashes [2][sha256.Size]byte
	for i, path := range []string{modelPath, modelPath + ".data"} {
		file, err := os.Open(path)
		if i == 1 && os.IsNotExist(err) {
			continue // ONNX also supports inline weights
		}
		if err != nil {
			return "", err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		hash.Sum(hashes[i][:0])
	}
	return filepath.Join(root, fmt.Sprintf("%x-%x", hashes[0], hashes[1])), nil
}
