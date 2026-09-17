// sandbox-action-fixture is a small conforming Handler used only by the
// Quick Start demo and Sandbox Actions E2E suite.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	actionapi "fast-sandbox/internal/protocol/action"
	"fast-sandbox/pkg/util/idgen"
)

type fixture struct {
	mu               sync.Mutex
	instanceID       string
	states           map[string]bindingState
	seen             map[string]struct{}
	removeDelayPorts map[string]struct{}
}

type bindingState struct {
	input string
	hooks map[actionapi.LifecycleHook]int64
}

func main() {
	instanceID, err := idgen.GenerateRequestID()
	if err != nil {
		log.Fatal(err)
	}
	handler := &fixture{
		instanceID: instanceID, states: make(map[string]bindingState), seen: make(map[string]struct{}),
		removeDelayPorts: stringSet(os.Getenv("SANDBOX_ACTION_FIXTURE_REMOVE_DELAY_PORTS")),
	}
	addresses := strings.Split(os.Getenv("SANDBOX_ACTION_FIXTURE_ADDRESSES"), ",")
	if len(addresses) == 1 && addresses[0] == "" {
		addresses = []string{"127.0.0.1:18080"}
	}
	for _, configuredAddress := range addresses {
		address := strings.TrimSpace(configuredAddress)
		mux := http.NewServeMux()
		mux.HandleFunc("/_fastlet/v1/actions/status", handler.status)
		mux.HandleFunc("/_fastlet/v1/actions", func(writer http.ResponseWriter, request *http.Request) {
			handler.invoke(address, writer, request)
		})
		go func() {
			log.Printf("sandbox Action fixture listening on %s instance=%s", address, instanceID)
			if err := http.ListenAndServe(address, mux); err != nil {
				log.Fatal(err)
			}
		}()
	}
	select {}
}

func (f *fixture) status(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(writer, http.StatusOK, actionapi.HandlerStatus{APIVersion: actionapi.APIVersion, Ready: true, InstanceID: f.instanceID})
}

func (f *fixture) invoke(address string, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var invocation actionapi.Request
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 128<<10))
	if err := decoder.Decode(&invocation); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if invocation.APIVersion != actionapi.APIVersion || invocation.InvocationID == "" || invocation.Sandbox.UID == "" || invocation.Revision.AttachmentID == "" {
		http.Error(writer, "invalid Action envelope", http.StatusBadRequest)
		return
	}
	targetPort := address[strings.LastIndex(address, ":")+1:]
	if invocation.Operation == actionapi.OperationRemoveBinding {
		if _, delayed := f.removeDelayPorts[targetPort]; delayed {
			// The key=value fields of the request log lines (here, below, and
			// in logInvocation) are asserted by the Sandbox Actions E2E suite
			// (sandbox_actions_test.go greps the fixture Pod logs); keep the
			// format stable or update the suite with it.
			log.Printf("targetPort=%s operation=%s sandboxUid=%s invocationId=%s injectedRemoveDelay=true", targetPort, invocation.Operation, invocation.Sandbox.UID, invocation.InvocationID)
			elapsed, cancelled := waitForCancellation(request.Context())
			log.Printf("targetPort=%s operation=%s sandboxUid=%s invocationId=%s injectedRemoveDelayComplete=true cancelled=%t elapsedMillis=%d", targetPort, invocation.Operation, invocation.Sandbox.UID, invocation.InvocationID, cancelled, elapsed.Milliseconds())
			return
		}
	}
	key := address + "/" + invocation.Sandbox.UID
	f.mu.Lock()
	if _, duplicate := f.seen[invocation.InvocationID]; duplicate {
		f.mu.Unlock()
		logInvocation(address, invocation, true)
		writer.WriteHeader(http.StatusOK)
		return
	}
	switch invocation.Operation {
	case actionapi.OperationSetBinding:
		if invocation.Binding == nil {
			f.mu.Unlock()
			http.Error(writer, "SET_BINDING requires binding payload", http.StatusBadRequest)
			return
		}
		if invocation.Binding.Input == nil {
			delete(f.states, key)
		} else {
			state := f.states[key]
			state.input = *invocation.Binding.Input
			if state.hooks == nil {
				state.hooks = make(map[actionapi.LifecycleHook]int64)
			}
			f.states[key] = state
		}
	case actionapi.OperationLifecycleHook:
		if invocation.Hook == nil {
			f.mu.Unlock()
			http.Error(writer, "LIFECYCLE_HOOK requires hook payload", http.StatusBadRequest)
			return
		}
		state, found := f.states[key]
		if !found {
			f.mu.Unlock()
			http.Error(writer, "Binding must be set before Hook", http.StatusConflict)
			return
		}
		if state.hooks == nil {
			state.hooks = make(map[actionapi.LifecycleHook]int64)
		}
		state.hooks[invocation.Hook.Name] = invocation.Hook.Sequence
		f.states[key] = state
	case actionapi.OperationRemoveBinding:
		if invocation.Binding != nil || invocation.Hook != nil {
			f.mu.Unlock()
			http.Error(writer, "REMOVE_BINDING does not accept a payload", http.StatusBadRequest)
			return
		}
		delete(f.states, key)
	default:
		f.mu.Unlock()
		http.Error(writer, "unsupported operation", http.StatusBadRequest)
		return
	}
	f.seen[invocation.InvocationID] = struct{}{}
	f.mu.Unlock()
	logInvocation(address, invocation, false)
	writer.WriteHeader(http.StatusOK)
}

func logInvocation(address string, invocation actionapi.Request, duplicate bool) {
	targetPort := address[strings.LastIndex(address, ":")+1:]
	input := "<none>"
	if invocation.Binding != nil {
		if invocation.Binding.Input == nil {
			input = "<nil>"
		} else {
			input = fmt.Sprintf("%q", *invocation.Binding.Input)
		}
	}
	hook := ""
	if invocation.Hook != nil {
		hook = fmt.Sprintf("%s/%d", invocation.Hook.Name, invocation.Hook.Sequence)
	}
	// Format asserted by the Actions E2E suite; see the note in invoke.
	log.Printf("targetPort=%s operation=%s input=%s hook=%s sandboxUid=%s generation=%d attachmentId=%s invocationId=%s duplicate=%t", targetPort, invocation.Operation, input, hook, invocation.Sandbox.UID, invocation.Revision.SpecGeneration, invocation.Revision.AttachmentID, invocation.InvocationID, duplicate)
}

func stringSet(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}

func waitForCancellation(ctx context.Context) (time.Duration, bool) {
	started := time.Now()
	select {
	case <-ctx.Done():
		return time.Since(started), true
	case <-time.After(30 * time.Second):
		return time.Since(started), false
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Print(fmt.Errorf("encode response: %w", err))
	}
}
