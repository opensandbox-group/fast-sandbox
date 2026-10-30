package action

// vault_transfer.go implements the Fastlet side of the driver-invoked egress
// state transfers (runtimecontract.StateMover): the subject's vault file
// moves across pause/resume as VAULT_EXPORT / VAULT_IMPORT (OSEP-0022 "Vault
// Lifecycle") and the recorded network policy file as POLICY_EXPORT /
// POLICY_IMPORT, both on the Pool-declared vault-capable Action Handler over
// the existing Pod-loopback Sandbox Actions channel — one handler serves both
// surfaces, so there is no second env knob. The envelope carries the bundle
// file PATH (snapshot.path) — the Handler copies/reads the file itself
// through its commit protocol, so no state bytes ever cross this process.
// The envelope fence is the Sandbox's currently registered fence, so a stale
// invocation is rejected by the Handler.
//
// The default handler name matches the OSEP-0022 fast-sandbox profile
// convention ("egress"); pools without that handler never transfer state
// files, and a successful export without a bundle file simply means the
// subject held no state of that kind.

import (
	"context"
	"fmt"
	"os"

	"k8s.io/klog/v2"

	actionapi "fast-sandbox/internal/protocol/action"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// vaultTransferEnvVar optionally overrides the name of the Pool-declared
// Action Handler that serves the egress vault and policy surfaces.
const vaultTransferEnvVar = "FAST_SANDBOX_VAULT_HANDLER"

// defaultVaultHandler is the conventional name of the egress Action Handler.
const defaultVaultHandler = "egress"

// SetVaultHandler configures the Pool-declared Action Handler name that
// serves the egress vault and policy surfaces. An empty name disables the
// transfers.
func (m *Manager) SetVaultHandler(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vaultHandler = name
}

// VaultHandler returns the configured vault handler name.
func (m *Manager) VaultHandler() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.vaultHandler
}

// VaultHandlerFromEnv resolves the vault handler name from the environment,
// defaulting to the egress convention.
func VaultHandlerFromEnv() string {
	if name := os.Getenv(vaultTransferEnvVar); name != "" {
		return name
	}
	return defaultVaultHandler
}

var _ runtimecontract.StateMover = (*Manager)(nil)

// ExportState copies the subject's state file of kind into the driver-provided
// bundle target (the StateMover entry point the driver invokes per kind).
func (m *Manager) ExportState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	switch kind {
	case runtimecontract.StateKindVault:
		return m.ExportVault(ctx, sandboxUID, bundlePath)
	case runtimecontract.StateKindPolicy:
		return m.ExportPolicy(ctx, sandboxUID, bundlePath)
	default:
		return fmt.Errorf("export state: unknown state kind %q", kind)
	}
}

// ImportState lands the bundle source file as the subject's state of kind,
// re-stamped by the Handler with the restored runtime fence.
func (m *Manager) ImportState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	switch kind {
	case runtimecontract.StateKindVault:
		return m.ImportVault(ctx, sandboxUID, bundlePath)
	case runtimecontract.StateKindPolicy:
		return m.ImportPolicy(ctx, sandboxUID, bundlePath)
	default:
		return fmt.Errorf("import state: unknown state kind %q", kind)
	}
}

// ExportVault copies the subject's vault file into the driver-provided
// bundle target. The Handler seals the subject on success; any non-2xx
// answer (unknown subject, stale fence, copy failure) is a transfer failure
// that fails the checkpoint.
func (m *Manager) ExportVault(ctx context.Context, sandboxUID, bundlePath string) error {
	return m.invokeStateTransfer(ctx, actionapi.OperationVaultExport, sandboxUID, bundlePath)
}

// ImportVault lands the bundle source file as the subject's vault,
// re-stamped by the Handler with the restored runtime fence. A failure is
// returned to the driver, which logs it and proceeds — the restored
// subject's vault then degrades (fail-closed, observable).
func (m *Manager) ImportVault(ctx context.Context, sandboxUID, bundlePath string) error {
	return m.invokeStateTransfer(ctx, actionapi.OperationVaultImport, sandboxUID, bundlePath)
}

// ExportPolicy copies the subject's recorded network policy file into the
// driver-provided bundle target. The same failure semantics as ExportVault:
// a non-2xx answer fails the snapshot so the egress-local file survives for
// the retry.
func (m *Manager) ExportPolicy(ctx context.Context, sandboxUID, bundlePath string) error {
	return m.invokeStateTransfer(ctx, actionapi.OperationPolicyExport, sandboxUID, bundlePath)
}

// ImportPolicy lands the bundle source file as the subject's recorded
// network policy, re-stamped by the Handler with the restored runtime fence.
// A failure is returned to the driver, which logs it and proceeds — the
// restored subject then keeps the binding-input policy of the SET_BINDING
// replay (the bootstrap fallback, observable).
func (m *Manager) ImportPolicy(ctx context.Context, sandboxUID, bundlePath string) error {
	return m.invokeStateTransfer(ctx, actionapi.OperationPolicyImport, sandboxUID, bundlePath)
}

// ExportTemplateState copies the subject's state file of kind into the
// bundle target WITHOUT sealing the subject (the template-carry semantics:
// the source keeps full relay CRUD — a template's completion path never
// imports, so a sealed export would lock it out). A Handler without the
// TemplateStateExport capability is a LOUD failure, never a silent skip:
// a snapshot must be a complete capture, and a state-less template must
// not pass as the source's state. Any Handler-side failure fails the
// snapshot for the same reason.
func (m *Manager) ExportTemplateState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	switch kind {
	case runtimecontract.StateKindVault:
		return m.invokeTemplateTransfer(ctx, actionapi.OperationVaultExport, sandboxUID, bundlePath, true)
	case runtimecontract.StateKindPolicy:
		return m.invokeTemplateTransfer(ctx, actionapi.OperationPolicyExport, sandboxUID, bundlePath, true)
	default:
		return fmt.Errorf("export template state: unknown state kind %q", kind)
	}
}

// ImportTemplateState lands a template-borne state record as the FRESH
// subject's seed (land-for-next-registration): a clone booted from a
// template whose bundle carries state starts with the template's state,
// re-stamped to the clone's registration at first SET_BINDING. Against a
// Handler without the capability the transfer fails and the CALLER decides
// the landing posture (the driver degrades the clone to the CRD seed — the
// bundle can only carry state when the exporting egress was capable).
func (m *Manager) ImportTemplateState(ctx context.Context, sandboxUID string, kind runtimecontract.StateKind, bundlePath string) error {
	switch kind {
	case runtimecontract.StateKindVault:
		return m.invokeTemplateTransfer(ctx, actionapi.OperationVaultImport, sandboxUID, bundlePath, false)
	case runtimecontract.StateKindPolicy:
		return m.invokeTemplateTransfer(ctx, actionapi.OperationPolicyImport, sandboxUID, bundlePath, false)
	default:
		return fmt.Errorf("import template state: unknown state kind %q", kind)
	}
}

// invokeTemplateTransfer sends one template-carry transfer invocation. The
// shape mirrors invokeStateTransfer, but the capability gate is the
// Handler's TemplateStateExport flag (the envelope's Seal=false and the
// land-for-next-registration import semantics only exist on handlers that
// declare it) and template exports carry Seal=false.
func (m *Manager) invokeTemplateTransfer(ctx context.Context, operation actionapi.Operation, sandboxUID, bundlePath string, seal bool) error {
	m.mu.RLock()
	name := m.vaultHandler
	handler, declared := m.byName[name]
	m.mu.RUnlock()
	if name == "" || !declared {
		return nil
	}
	if m.apiVersion == actionapi.APIVersionV2 {
		return fmt.Errorf("%s is not supported by the strict Actions v2 management slice", operation)
	}
	invocation, err := m.stateInvocation(ctx, operation, name, sandboxUID, bundlePath)
	if err != nil {
		return err
	}
	status, err := m.caller.Status(ctx, handler.TargetHTTPPort, m.apiVersion)
	if err != nil {
		return fmt.Errorf("%s template state transfer capability probe for Sandbox %s: %w", operation, sandboxUID, err)
	}
	if !status.TemplateStateExport {
		// Fail LOUD: a snapshot must be a complete capture. Silently
		// skipping would publish a state-less template as if it were the
		// source's state. The fix is an egress that honors
		// SnapshotTransfer.Seal=false + land-for-next-registration and
		// reports templateStateExport=true (opensandbox#2312).
		return fmt.Errorf("%s template state transfer for Sandbox %s: Action Handler %s does not support seal-free template carry (templateStateExport=false); the template snapshot is refused rather than published without its state — upgrade the egress per opensandbox#2312",
			operation, sandboxUID, name)
	}
	if seal {
		falseValue := false
		invocation.Snapshot.Seal = &falseValue
	}
	if err := m.caller.Invoke(ctx, handler.TargetHTTPPort, invocation); err != nil {
		return fmt.Errorf("%s template state transfer for Sandbox %s: %w", operation, sandboxUID, err)
	}
	return nil
}

// stateTransferCapable reports whether a Handler status declares the
// capability of one state kind's operation pair. Each kind probes its own
// flag: a Handler may serve the policy surface without vault persistence
// (and vice versa).
func stateTransferCapable(status actionapi.HandlerStatus, operation actionapi.Operation) bool {
	switch operation {
	case actionapi.OperationVaultExport, actionapi.OperationVaultImport:
		return status.VaultTransfer
	case actionapi.OperationPolicyExport, actionapi.OperationPolicyImport:
		return status.PolicyTransfer
	default:
		return false
	}
}

// stateKindOfOperation names the state kind an operation pair belongs to
// (log lines use it; unknown operations degrade to the operation string).
func stateKindOfOperation(operation actionapi.Operation) string {
	switch operation {
	case actionapi.OperationVaultExport, actionapi.OperationVaultImport:
		return string(runtimecontract.StateKindVault)
	case actionapi.OperationPolicyExport, actionapi.OperationPolicyImport:
		return string(runtimecontract.StateKindPolicy)
	default:
		return string(operation)
	}
}

// invokeStateTransfer sends one state-transfer invocation on the actions
// channel. Transfer is disabled (a silent no-op) without a configured
// handler or when the Pool does not declare it; a Sandbox without a
// registered Action state cannot be transferred.
func (m *Manager) invokeStateTransfer(ctx context.Context, operation actionapi.Operation, sandboxUID, bundlePath string) error {
	m.mu.RLock()
	name := m.vaultHandler
	handler, declared := m.byName[name]
	m.mu.RUnlock()
	if name == "" || !declared {
		// The Pool cannot hold egress state at all: nothing to transfer.
		return nil
	}
	if m.apiVersion == actionapi.APIVersionV2 {
		return fmt.Errorf("%s is not supported by the strict Actions v2 management slice", operation)
	}
	invocation, err := m.stateInvocation(ctx, operation, name, sandboxUID, bundlePath)
	if err != nil {
		return err
	}
	// Capability probe before the invocation, per kind: a Handler without
	// the kind's transfer (older build predating the operations, or a build
	// without persistence for the kind configured) must not fail the
	// checkpoint — skipping is the pre-transfer semantics, and the resumed
	// subject then keeps the pre-transfer behavior (no vault; the binding
	// input as the only policy). An unreachable Handler is NOT a capability
	// answer: the probe failure fails the checkpoint so the egress-local
	// files survive for the retry.
	status, err := m.caller.Status(ctx, handler.TargetHTTPPort, m.apiVersion)
	if err != nil {
		return fmt.Errorf("%s state transfer capability probe for Sandbox %s: %w", operation, sandboxUID, err)
	}
	if !stateTransferCapable(status, operation) {
		klog.Infof("state transfer skipped: Action Handler %s carries no %s state (%s transfer not declared); proceeding without it",
			name, stateKindOfOperation(operation), operation)
		return nil
	}
	if err := m.caller.Invoke(ctx, handler.TargetHTTPPort, invocation); err != nil {
		return fmt.Errorf("%s state transfer for Sandbox %s: %w", operation, sandboxUID, err)
	}
	return nil
}

// stateInvocation builds the transfer envelope from the Sandbox's currently
// registered fence: the Handler matches it against the registration, so a
// stale invocation (replaced runtime, superseded attachment) is rejected
// instead of touching the wrong subject slice. The fence read takes
// state.mu like every established reader; the op token serializes against
// other invocation flows but NOT against the writers (registerDesired
// mutates both fields under state.mu before the token is acquired and on
// the RegisterDesired path).
func (m *Manager) stateInvocation(ctx context.Context, operation actionapi.Operation, handlerName, sandboxUID, bundlePath string) (actionapi.Request, error) {
	m.mu.RLock()
	state := m.states[sandboxUID]
	m.mu.RUnlock()
	if state == nil {
		return actionapi.Request{}, fmt.Errorf("%s state transfer: Sandbox %s has no registered Action state", operation, sandboxUID)
	}
	if !state.lockOperations(ctx) {
		return actionapi.Request{}, ctx.Err()
	}
	state.mu.RLock()
	attachment := state.attachment
	generation := state.desiredGeneration
	state.mu.RUnlock()
	state.unlockOperations()
	instance, err := m.handlerInstance(handlerName)
	if err != nil {
		instance = ""
	}
	request := buildRequest(operation, stableInvocationID(operation, handlerName, instance, generation, attachment, "", bundlePath), generation, attachment)
	request.Snapshot = &actionapi.SnapshotTransfer{Path: bundlePath}
	return request, nil
}
