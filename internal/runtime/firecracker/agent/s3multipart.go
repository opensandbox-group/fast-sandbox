package agent

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	maxSinglePUTSize     = 5_000_000_000
	defaultPartSize      = 64 << 20
	maxMultipartParts    = 10_000
	maxMultipartPartSize = 5 << 30
	multipartAbortBudget = 15 * time.Second
)

type multipartPart struct {
	PartNumber int
	ETag       string
}

func multipartPartSize(size int64) int64 {
	return max(defaultPartSize, (size-1)/maxMultipartParts+1)
}

func (c *s3Client) putMultipart(ctx context.Context, storeKey string, file io.ReaderAt, size, partSize int64) (err error) {
	if !c.writable() {
		return ErrNotWritable
	}
	if partSize > maxMultipartPartSize {
		return fmt.Errorf("object exceeds S3 multipart capacity: %d bytes", size)
	}
	key := storeKey
	if c.prefix != "" {
		key = c.prefix + "/" + key
	}
	address, err := c.objectURL(key)
	if err != nil {
		return err
	}
	target, err := url.Parse(address)
	if err != nil {
		return err
	}

	var uploadID string
	err = c.retryMultipart(ctx, func() error {
		var result struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			UploadID string   `xml:"UploadId"`
		}
		_, err := c.multipartRequest(ctx, http.MethodPost, *target, [][2]string{{"uploads", ""}}, nil, 0, &result)
		if err != nil {
			return err
		}
		if result.UploadID == "" {
			return fmt.Errorf("S3 returned an empty multipart upload ID")
		}
		uploadID = result.UploadID
		return nil
	})
	if err != nil {
		return fmt.Errorf("initiate multipart upload for %s: %w", key, err)
	}

	uploadQuery := [][2]string{{"uploadId", uploadID}}
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), multipartAbortBudget)
		defer cancel()
		abortErr := c.retryMultipart(cleanupCtx, func() error {
			_, abortErr := c.multipartRequest(cleanupCtx, http.MethodDelete, *target, uploadQuery, nil, 0, nil)
			var status *httpError
			if errors.As(abortErr, &status) && status.StatusCode == http.StatusNotFound {
				return nil
			}
			return abortErr
		})
		if abortErr != nil {
			err = errors.Join(err, fmt.Errorf("abort multipart upload for %s: %w", key, abortErr))
		}
	}()

	parts := make([]multipartPart, 0, (size-1)/partSize+1)
	for offset := int64(0); offset < size; {
		partNumber := len(parts) + 1
		length := min(partSize, size-offset)
		query := [][2]string{{"partNumber", strconv.Itoa(partNumber)}, {"uploadId", uploadID}}
		var etag string
		err = c.retryMultipart(ctx, func() error {
			part := io.NewSectionReader(file, offset, length)
			header, err := c.multipartRequest(ctx, http.MethodPut, *target, query, part, length, nil)
			if err != nil {
				return err
			}
			etag = header.Get("ETag")
			if etag == "" {
				return fmt.Errorf("S3 returned an empty ETag")
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("upload S3 part %d for %s: %w", partNumber, key, err)
		}
		parts = append(parts, multipartPart{PartNumber: partNumber, ETag: etag})
		offset += length
	}

	request, err := xml.Marshal(struct {
		XMLName xml.Name        `xml:"CompleteMultipartUpload"`
		Parts   []multipartPart `xml:"Part"`
	}{Parts: parts})
	if err != nil {
		return fmt.Errorf("encode multipart completion for %s: %w", key, err)
	}
	err = c.retryMultipart(ctx, func() error {
		var result struct {
			XMLName xml.Name
			Code    string
			Message string
		}
		_, err := c.multipartRequest(ctx, http.MethodPost, *target, uploadQuery, bytes.NewReader(request), int64(len(request)), &result)
		if err != nil {
			return err
		}
		// S3 may return an error inside an HTTP 200 completion response.
		switch result.XMLName.Local {
		case "CompleteMultipartUploadResult":
			return nil
		case "Error":
			switch result.Code {
			case "InternalError", "ServiceUnavailable", "SlowDown", "RequestTimeout":
				return &httpError{StatusCode: http.StatusServiceUnavailable, Body: result.Code}
			default:
				return &httpError{StatusCode: http.StatusBadRequest, Body: result.Code + ": " + result.Message}
			}
		default:
			return fmt.Errorf("unexpected multipart completion response %q", result.XMLName.Local)
		}
	})
	if err != nil {
		return fmt.Errorf("complete multipart upload for %s: %w", key, err)
	}
	return nil
}

func (c *s3Client) multipartRequest(ctx context.Context, method string, target url.URL, query [][2]string, body io.Reader, size int64, result any) (http.Header, error) {
	target.RawQuery = canonicalQueryString(query)
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	request.ContentLength = size
	if method == http.MethodPost && body != nil {
		request.Header.Set("Content-Type", "application/xml")
	}
	c.signWrite(request)
	response, err := c.putHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &httpError{StatusCode: response.StatusCode, Body: string(payload)}
	}
	if result != nil {
		if err := xml.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
			return nil, err
		}
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Header, nil
}

func (c *s3Client) retryMultipart(ctx context.Context, attempt func() error) error {
	var err error
	backoff := c.retryDelay
	for n := 0; n <= s3RetryAttempts; n++ {
		if n > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = attempt()
		var status *httpError
		if err == nil || (errors.As(err, &status) && status.StatusCode < 500 && status.StatusCode != http.StatusTooManyRequests) {
			return err
		}
	}
	return err
}
