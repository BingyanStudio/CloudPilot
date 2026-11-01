package runtime

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// collectOutput stages and validates artifacts before publishing their local paths.
func collectOutput(dataDir string, runID uint, redact func(string) string, copyOutput func(string) error) ([]model.Artifact, error) {
	base, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(base, "runs", strconv.FormatUint(uint64(runID), 10))
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, err
	}
	destination, err := os.MkdirTemp(runDir, "artifacts-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	if err := copyOutput(destination); err != nil {
		return nil, err
	}
	artifacts, err := readArtifacts(destination, runID, redact)
	if err != nil {
		return nil, err
	}
	keep = true
	return artifacts, nil
}

func extractOutput(reader io.Reader, destination string) error {
	archive := tar.NewReader(io.LimitReader(reader, maxOutputSize+(4<<20)))
	var total int64
	files := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if !filepath.IsLocal(name) || strings.Contains(header.Name, "\\") {
			return errors.New("unsafe output path")
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += header.Size
			if files > maxArtifacts+2 || header.Size > maxFileSize || total > maxOutputSize {
				return errors.New("worker output exceeds artifact limits")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, archive, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return errors.New("worker output may only contain regular files and directories")
		}
	}
}

type manifestEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

func readArtifacts(directory string, runID uint, redact func(string) string) ([]model.Artifact, error) {
	var result []model.Artifact
	manifest, err := readSmallFile(filepath.Join(directory, "artifacts.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		var entries struct {
			Artifacts []manifestEntry `json:"artifacts"`
		}
		if err := json.Unmarshal(manifest, &entries); err != nil {
			return nil, fmt.Errorf("invalid artifact manifest: %w", err)
		}
		if len(entries.Artifacts) > maxArtifacts {
			return nil, errors.New("too many artifacts")
		}
		for _, entry := range entries.Artifacts {
			artifact := model.Artifact{RunID: runID, Kind: entry.Kind, Name: redact(entry.Name), URL: redact(entry.URL), Content: redact(entry.Content), ContentType: entry.ContentType}
			if artifact.Name == "" || len(artifact.Name) > 255 {
				return nil, errors.New("artifact name must contain 1 to 255 bytes")
			}
			switch entry.Kind {
			case "file":
				if !filepath.IsLocal(entry.Path) || entry.Path == "." || strings.Contains(entry.Path, "\\") {
					return nil, errors.New("unsafe artifact file path")
				}
				artifact.Path = filepath.Join(directory, entry.Path)
				info, err := os.Lstat(artifact.Path)
				if err != nil || !info.Mode().IsRegular() {
					return nil, errors.New("artifact file is missing or not regular")
				}
				artifact.Size = info.Size()
				if artifact.ContentType == "" {
					artifact.ContentType = "application/octet-stream"
				}
			case "pr", "issue":
				parsed, err := url.Parse(artifact.URL)
				if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
					return nil, errors.New("artifact URL must be HTTP or HTTPS without credentials")
				}
			case "text":
				if len(artifact.Content) > maxTextSize {
					return nil, errors.New("text artifact is too large")
				}
			default:
				return nil, fmt.Errorf("unsupported artifact kind %q", entry.Kind)
			}
			result = append(result, artifact)
		}
	}
	summary, err := readSmallFile(filepath.Join(directory, "summary.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(bytes.TrimSpace(summary)) > 0 {
		result = append(result, model.Artifact{RunID: runID, Kind: "text", Name: "Agent response", Content: redact(string(summary)), ContentType: "text/markdown"})
	}
	return result, nil
}
func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, maxTextSize+1))
	if err == nil && len(value) > maxTextSize {
		return nil, errors.New("artifact text exceeds 1 MiB")
	}
	return value, err
}
