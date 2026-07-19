package sandboxclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ExecdPort uint32 = 44772

type RouteResolver interface {
	Resolve(context.Context, SandboxRef, uint32) (Route, error)
}

// ExecdAdapter implements the OpenSandbox Execd wire protocol over a route
// returned by EndpointResolver. Authentication of the injected component is
// internal to Fastlet Proxy; callers only receive the short-lived route token.
type ExecdAdapter struct {
	Resolver   RouteResolver
	HTTPClient *http.Client
	Port       uint32
}

type RunCommandRequest struct {
	Command    string            `json:"command"`
	CWD        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"`
	Timeout    time.Duration     `json:"-"`
	UID        *int32            `json:"uid,omitempty"`
	GID        *int32            `json:"gid,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
}

func (r RunCommandRequest) MarshalJSON() ([]byte, error) {
	type wireRequest struct {
		Command    string            `json:"command"`
		CWD        string            `json:"cwd,omitempty"`
		Background bool              `json:"background,omitempty"`
		Timeout    int64             `json:"timeout,omitempty"`
		UID        *int32            `json:"uid,omitempty"`
		GID        *int32            `json:"gid,omitempty"`
		Envs       map[string]string `json:"envs,omitempty"`
	}
	if r.Command == "" {
		return nil, errors.New("command is required")
	}
	if r.Timeout < 0 {
		return nil, errors.New("command timeout must not be negative")
	}
	return json.Marshal(wireRequest{
		Command: r.Command, CWD: r.CWD, Background: r.Background,
		Timeout: r.Timeout.Milliseconds(), UID: r.UID, GID: r.GID, Envs: r.Envs,
	})
}

type OutputMessage struct {
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
}

type ExecutionError struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Timestamp int64    `json:"timestamp"`
	Traceback []string `json:"traceback"`
}

type Execution struct {
	ID       string
	Stdout   []OutputMessage
	Stderr   []OutputMessage
	Error    *ExecutionError
	ExitCode *int
	Complete bool
}

type ExecutionHandlers struct {
	OnStdout         func(OutputMessage) error
	OnStderr         func(OutputMessage) error
	OnError          func(ExecutionError) error
	OnComplete       func() error
	SkipAccumulation bool
}

type streamEvent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     *struct {
		Name      string   `json:"ename,omitempty"`
		Value     string   `json:"evalue,omitempty"`
		Traceback []string `json:"traceback,omitempty"`
	} `json:"error,omitempty"`
	Name      string   `json:"ename,omitempty"`
	Value     string   `json:"evalue,omitempty"`
	Traceback []string `json:"traceback,omitempty"`
}

func (a *ExecdAdapter) RunCommand(ctx context.Context, sandbox SandboxRef, command RunCommandRequest, handlers *ExecutionHandlers) (Execution, error) {
	payload, err := json.Marshal(command)
	if err != nil {
		return Execution{}, err
	}
	response, err := a.do(ctx, sandbox, http.MethodPost, "/command", nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		return Execution{}, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return Execution{}, err
	}
	execution, count, err := consumeSSE(ctx, response.Body, handlers)
	if err != nil {
		return execution, err
	}
	if count == 0 {
		return execution, errors.New("execd returned an empty event stream")
	}
	if !command.Background && !execution.Complete && execution.Error == nil {
		return execution, errors.New("execd command stream ended without a completion event")
	}
	return execution, nil
}

func consumeSSE(ctx context.Context, body io.Reader, handlers *ExecutionHandlers) (Execution, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var execution Execution
	var data []string
	count := 0
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		count++
		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			message := OutputMessage{Text: payload}
			if handlers == nil || !handlers.SkipAccumulation {
				execution.Stdout = append(execution.Stdout, message)
			}
			if handlers != nil && handlers.OnStdout != nil {
				return handlers.OnStdout(message)
			}
			return nil
		}
		switch event.Type {
		case "init":
			execution.ID = event.Text
		case "stdout":
			message := OutputMessage{Text: event.Text, Timestamp: event.Timestamp}
			if handlers == nil || !handlers.SkipAccumulation {
				execution.Stdout = append(execution.Stdout, message)
			}
			if handlers != nil && handlers.OnStdout != nil {
				return handlers.OnStdout(message)
			}
		case "stderr":
			message := OutputMessage{Text: event.Text, Timestamp: event.Timestamp}
			if handlers == nil || !handlers.SkipAccumulation {
				execution.Stderr = append(execution.Stderr, message)
			}
			if handlers != nil && handlers.OnStderr != nil {
				return handlers.OnStderr(message)
			}
		case "error":
			executionError := ExecutionError{Name: event.Name, Value: event.Value, Timestamp: event.Timestamp, Traceback: event.Traceback}
			if event.Error != nil {
				executionError.Name, executionError.Value, executionError.Traceback = event.Error.Name, event.Error.Value, event.Error.Traceback
			}
			execution.Error = &executionError
			if event.ExitCode != nil {
				execution.ExitCode = event.ExitCode
			} else if code, err := strconv.Atoi(executionError.Value); err == nil {
				execution.ExitCode = &code
			}
			if handlers != nil && handlers.OnError != nil {
				return handlers.OnError(executionError)
			}
		case "execution_complete":
			execution.Complete = true
			if event.ExitCode != nil {
				execution.ExitCode = event.ExitCode
			} else if execution.ExitCode == nil && execution.Error == nil {
				zero := 0
				execution.ExitCode = &zero
			}
			if handlers != nil && handlers.OnComplete != nil {
				return handlers.OnComplete()
			}
		case "ping", "status", "result", "execution_count":
		default:
			if event.Text != "" {
				message := OutputMessage{Text: event.Text, Timestamp: event.Timestamp}
				if handlers == nil || !handlers.SkipAccumulation {
					execution.Stdout = append(execution.Stdout, message)
				}
				if handlers != nil && handlers.OnStdout != nil {
					return handlers.OnStdout(message)
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return execution, count, err
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return execution, count, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			data = append(data, line)
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if ok && field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return execution, count, fmt.Errorf("read execd event stream: %w", err)
	}
	if err := dispatch(); err != nil {
		return execution, count, err
	}
	return execution, count, nil
}

type FileInfo struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
	Mode       int    `json:"mode"`
}

func (a *ExecdAdapter) Stat(ctx context.Context, sandbox SandboxRef, path string) (FileInfo, error) {
	query := url.Values{"path": []string{path}}
	response, err := a.do(ctx, sandbox, http.MethodGet, "/files/info", query, nil, "")
	if err != nil {
		return FileInfo{}, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return FileInfo{}, err
	}
	result := map[string]FileInfo{}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return FileInfo{}, fmt.Errorf("decode execd file info: %w", err)
	}
	info, ok := result[path]
	if !ok {
		return FileInfo{}, fmt.Errorf("execd response omitted file %q", path)
	}
	return info, nil
}

func (a *ExecdAdapter) List(ctx context.Context, sandbox SandboxRef, path string) ([]FileInfo, error) {
	query := url.Values{"path": []string{path}}
	response, err := a.do(ctx, sandbox, http.MethodGet, "/directories/list", query, nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return nil, err
	}
	var result []FileInfo
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode execd directory list: %w", err)
	}
	return result, nil
}

func (a *ExecdAdapter) Download(ctx context.Context, sandbox SandboxRef, path string, destination io.Writer) (int64, error) {
	query := url.Values{"path": []string{path}}
	response, err := a.do(ctx, sandbox, http.MethodGet, "/files/download", query, nil, "")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return 0, err
	}
	written, err := io.Copy(destination, response.Body)
	if err != nil {
		return written, fmt.Errorf("download execd file: %w", err)
	}
	return written, nil
}

func (a *ExecdAdapter) Upload(ctx context.Context, sandbox SandboxRef, path string, source io.Reader, mode int) error {
	if source == nil || path == "" {
		return errors.New("upload source and destination path are required")
	}
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	contentType := multipartWriter.FormDataContentType()
	go func() {
		metadata, err := json.Marshal(map[string]any{"path": path, "mode": octalDigits(mode)})
		if err == nil {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
			header.Set("Content-Type", "application/json")
			var part io.Writer
			part, err = multipartWriter.CreatePart(header)
			if err == nil {
				_, err = part.Write(metadata)
			}
		}
		if err == nil {
			var part io.Writer
			part, err = multipartWriter.CreateFormFile("file", filepath.Base(path))
			if err == nil {
				_, err = io.Copy(part, source)
			}
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = pipeWriter.CloseWithError(err)
	}()
	response, err := a.do(ctx, sandbox, http.MethodPost, "/files/upload", nil, pipeReader, contentType)
	if err != nil {
		_ = pipeReader.CloseWithError(err)
		return err
	}
	defer response.Body.Close()
	return requireSuccess(response)
}

func (a *ExecdAdapter) MakeDir(ctx context.Context, sandbox SandboxRef, path string, mode int) error {
	payload, err := json.Marshal(map[string]map[string]int{path: {"mode": octalDigits(mode)}})
	if err != nil {
		return err
	}
	response, err := a.do(ctx, sandbox, http.MethodPost, "/directories", nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return requireSuccess(response)
}

func (a *ExecdAdapter) Delete(ctx context.Context, sandbox SandboxRef, path string, directory bool) error {
	endpoint := "/files"
	if directory {
		endpoint = "/directories"
	}
	response, err := a.do(ctx, sandbox, http.MethodDelete, endpoint, url.Values{"path": []string{path}}, nil, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return requireSuccess(response)
}

func (a *ExecdAdapter) do(ctx context.Context, sandbox SandboxRef, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	if a == nil || a.Resolver == nil {
		return nil, errors.New("Execd adapter route resolver is not configured")
	}
	port := a.Port
	if port == 0 {
		port = ExecdPort
	}
	route, err := a.Resolver.Resolve(ctx, sandbox, port)
	if err != nil {
		return nil, err
	}
	requestURL, err := route.RequestURL(path, query)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	route.ApplyHeaders(request)
	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call execd through Sandbox Proxy: %w", err)
	}
	return response, nil
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("execd returned HTTP %d: %s", e.StatusCode, e.Body)
}

func requireSuccess(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	return &APIError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body))}
}

func octalDigits(mode int) int {
	value, err := strconv.Atoi(strconv.FormatInt(int64(mode), 8))
	if err != nil {
		return mode
	}
	return value
}

// ShellJoin produces one POSIX shell command without relying on a shell in the
// control plane. Execd intentionally accepts a command string.
func ShellJoin(arguments []string) (string, error) {
	if len(arguments) == 0 {
		return "", errors.New("at least one command argument is required")
	}
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		if argument == "" {
			quoted[index] = "''"
			continue
		}
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " "), nil
}
