package runtime

import (
	"errors"
	"strings"
)

var (
	// ErrUnsupportedRuntime 不支持的运行时类型
	ErrUnsupportedRuntime = errors.New("unsupported container runtime")

	// ErrSandboxNotFound sandbox 不存在
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxAlreadyExists sandbox 已存在
	ErrSandboxAlreadyExists = errors.New("sandbox already exists")

	// ErrRuntimeNotInitialized 运行时未初始化
	ErrRuntimeNotInitialized = errors.New("runtime not initialized")

	// ErrRuntimeCapabilityUnavailable indicates that a configured runtime cannot
	// safely accept Sandboxes on the current node.
	ErrRuntimeCapabilityUnavailable = errors.New("runtime capability unavailable")

	// ErrNetworkUnavailable indicates that Fastlet has no clean, validated
	// runtime network resource available for a new Sandbox.
	ErrNetworkUnavailable = errors.New("sandbox network unavailable")

	// ErrSandboxProfileMismatch rejects a request that attempts to override the
	// RuntimeProfile or fixed Pool ResourceProfile assigned to this Fastlet.
	ErrSandboxProfileMismatch = errors.New("sandbox profile mismatch")

	// ErrInvalidConfig 无效的配置
	ErrInvalidConfig = errors.New("invalid sandbox config")
)

type Errors []error

func NewErrors() Errors {
	return make([]error, 0)
}

func (e *Errors) Add(err error) {
	if nil == *e {
		*e = NewErrors()
	}
	if nil == err {
		return
	}
	*e = append(*e, err)
}

func JoinErrors(errs ...error) error {
	es := NewErrors()
	for _, err := range errs {
		es.Add(err)
	}
	return es.Error()
}

func (e *Errors) Empty() bool {
	return 0 == len(*e)
}

func (e *Errors) String() string {
	if nil == *e {
		return ""
	}
	errors := []error(*e)
	if 0 == len(errors) {
		return ""
	}
	var b strings.Builder
	for i := range errors {
		b.WriteString(errors[i].Error())
		if i != (len(errors) - 1) {
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

func (e *Errors) Error() error {
	if nil == *e {
		return nil
	}
	str := e.String()
	if "" == str {
		return nil
	}
	return errors.New(str)
}
