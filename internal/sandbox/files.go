package sandbox

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

func (s *Sandbox) headers(header http.Header) {
	if s.accessToken != "" {
		header.Set("X-Access-Token", s.accessToken)
	}
	if s.client.Provider == "novita" {
		header.Set("Novita-Sandbox-Id", s.ID)
		header.Set("Novita-Sandbox-Port", "49983")
	}
}

// Upload writes a file through envd's multipart endpoint without buffering it.
func (s *Sandbox) Upload(ctx context.Context, path string, source io.Reader) error {
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	defer reader.Close()
	go func() {
		part, err := multipartWriter.CreateFormFile("file", path)
		if err == nil {
			_, err = io.Copy(part, source)
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.envdURL+"/files?"+url.Values{"path": {path}}.Encode(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	s.headers(req.Header)
	response, err := s.client.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("upload sandbox file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{response.StatusCode, "upload file"}
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return err
}

// Download streams a file; callers must bound its size and close the response.
func (s *Sandbox) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.envdURL+"/files?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	s.headers(req.Header)
	response, err := s.client.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download sandbox file: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, &HTTPError{response.StatusCode, "download file"}
	}
	return response.Body, nil
}
