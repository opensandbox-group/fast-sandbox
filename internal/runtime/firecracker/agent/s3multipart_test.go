package agent

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	agentprotocol "fast-sandbox/internal/runtime/firecracker/agent/protocol"
)

func TestPublishCheckpointUsesMultipartBeforeManifest(t *testing.T) {
	dir := stageArtifactSet(t)
	const rootfsSize = maxSinglePUTSize + 1
	require.NoError(t, os.Truncate(filepath.Join(dir, "rootfs.ext4"), rootfsSize))

	client := testS3Client(t, &fakeStore{bucket: testBucket, prefix: testPrefix})
	client.writeAccessKey, client.writeSecretKey = "writer", "secret"
	var uploads []string
	var parts []multipartPart
	var uploadedBytes int64
	const uploadID = "parse+/=this&"
	client.putHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		name := filepath.Base(r.URL.Path)
		require.Contains(t, r.Header.Get("Authorization"), "Credential=writer/")
		if r.Body != nil {
			defer r.Body.Close()
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			require.Equal(t, "rootfs.ext4", name)
			require.Zero(t, uploadedBytes)
			return fakeS3Reply(http.StatusOK, "<InitiateMultipartUploadResult><UploadId>parse+/=this&amp;</UploadId></InitiateMultipartUploadResult>"), nil
		case r.Method == http.MethodPut && r.URL.Query().Has("partNumber"):
			require.Equal(t, "rootfs.ext4", name)
			require.Equal(t, uploadID, r.URL.Query().Get("uploadId"))
			partNumber := len(parts) + 1
			require.Equal(t, strconv.Itoa(partNumber), r.URL.Query().Get("partNumber"))
			require.Equal(t, "partNumber="+strconv.Itoa(partNumber)+"&uploadId=parse%2B%2F%3Dthis%26", r.URL.RawQuery)
			require.Equal(t, min(int64(defaultPartSize), rootfsSize-uploadedBytes), r.ContentLength)
			uploadedBytes += r.ContentLength
			response := fakeS3Reply(http.StatusOK, "")
			etag := `"part-` + strconv.Itoa(partNumber) + `"`
			response.Header.Set("ETag", etag)
			parts = append(parts, multipartPart{PartNumber: partNumber, ETag: etag})
			return response, nil
		case r.Method == http.MethodPost && r.URL.Query().Has("uploadId"):
			require.Equal(t, "rootfs.ext4", name)
			require.Equal(t, uploadID, r.URL.Query().Get("uploadId"))
			require.Equal(t, int64(rootfsSize), uploadedBytes)
			var completion struct {
				XMLName xml.Name        `xml:"CompleteMultipartUpload"`
				Parts   []multipartPart `xml:"Part"`
			}
			require.NoError(t, xml.NewDecoder(r.Body).Decode(&completion))
			require.Equal(t, parts, completion.Parts)
			uploads = append(uploads, name)
			return fakeS3Reply(http.StatusOK, " \n<CompleteMultipartUploadResult/>"), nil
		case r.Method == http.MethodPut:
			require.Empty(t, r.URL.RawQuery)
			info, err := os.Stat(filepath.Join(dir, name))
			require.NoError(t, err)
			require.Equal(t, info.Size(), r.ContentLength)
			uploads = append(uploads, name)
			return fakeS3Reply(http.StatusOK, ""), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			return fakeS3Reply(http.StatusBadRequest, ""), nil
		}
	})}

	result, err := (&Client{s3: client}).PublishImage(context.Background(), agentprotocol.PublishKindCheckpoint, "", dir)
	require.NoError(t, err)
	require.Contains(t, result.ManifestRef, "/manifest.json")
	require.Equal(t, []string{"rootfs.ext4", manifestVMStateName, manifestMemoryName, "SHA256SUMS", publishManifestName}, uploads)
}

func TestS3MultipartRetriesTransientErrors(t *testing.T) {
	client := testS3Client(t, &fakeStore{bucket: testBucket, prefix: testPrefix})
	client.writeAccessKey, client.writeSecretKey = "writer", "secret"
	var initiates, attempts, completes int
	client.putHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			defer r.Body.Close()
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			initiates++
			if initiates == 1 {
				return fakeS3Reply(http.StatusServiceUnavailable, "retry"), nil
			}
			return fakeS3Reply(http.StatusOK, "<InitiateMultipartUploadResult><UploadId>123</UploadId></InitiateMultipartUploadResult>"), nil
		case r.Method == http.MethodPut:
			payload, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			if r.URL.Query().Get("partNumber") == "1" {
				attempts++
				require.Equal(t, "abc", string(payload))
				if attempts == 1 {
					return fakeS3Reply(http.StatusBadGateway, "retry"), nil
				}
			} else {
				require.Equal(t, "d", string(payload))
			}
			response := fakeS3Reply(http.StatusOK, "")
			response.Header.Set("ETag", `"etag"`)
			return response, nil
		case r.Method == http.MethodPost:
			completes++
			if completes == 1 {
				return fakeS3Reply(http.StatusOK, " \n<Error><Code>SlowDown</Code></Error>"), nil
			}
			return fakeS3Reply(http.StatusOK, "<CompleteMultipartUploadResult/>"), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			return fakeS3Reply(http.StatusBadRequest, ""), nil
		}
	})}
	require.NoError(t, client.putMultipart(context.Background(), "object", strings.NewReader("abcd"), 4, 3))
	require.Equal(t, 2, initiates)
	require.Equal(t, 2, attempts)
	require.Equal(t, 2, completes)
}

func TestS3MultipartFailureAbortsWithoutManifest(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "permanent completion error"
		if canceled {
			name = "canceled part"
		}
		t.Run(name, func(t *testing.T) {
			dir := stageArtifactSet(t)
			require.NoError(t, os.Truncate(filepath.Join(dir, "rootfs.ext4"), maxSinglePUTSize+1))

			client := testS3Client(t, &fakeStore{bucket: testBucket, prefix: testPrefix})
			client.writeAccessKey, client.writeSecretKey = "writer", "secret"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var aborts int
			client.putHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					defer r.Body.Close()
				}
				require.Equal(t, "rootfs.ext4", filepath.Base(r.URL.Path))
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					return fakeS3Reply(http.StatusOK, "<InitiateMultipartUploadResult><UploadId>123</UploadId></InitiateMultipartUploadResult>"), nil
				case r.Method == http.MethodPut:
					if canceled {
						cancel()
						return nil, errors.New("transport interrupted")
					}
					response := fakeS3Reply(http.StatusOK, "")
					response.Header.Set("ETag", `"etag"`)
					return response, nil
				case r.Method == http.MethodPost:
					return fakeS3Reply(http.StatusOK, "<Error><Code>InvalidPart</Code></Error>"), nil
				case r.Method == http.MethodDelete:
					aborts++
					require.NoError(t, r.Context().Err())
					require.Equal(t, "123", r.URL.Query().Get("uploadId"))
					return fakeS3Reply(http.StatusNoContent, ""), nil
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					return fakeS3Reply(http.StatusBadRequest, ""), nil
				}
			})}
			result, err := (&Client{s3: client}).PublishImage(ctx, agentprotocol.PublishKindCheckpoint, "", dir)
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorContains(t, err, "InvalidPart")
			}
			require.Empty(t, result.ManifestRef)
			require.Equal(t, 1, aborts)
		})
	}
}

func fakeS3Reply(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
