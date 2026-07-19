package api

// The exported seam between the route table and the OpenAPI/api.ts generator. gen.go
// (build-ignored) runs RouteMetas() + ParseRouteDocs() and hands both to internal/openapi;
// see docs/openapi.md. The directive below is the whole trigger:
//
//go:generate go run gen.go

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

func (k scopeKind) String() string {
	switch k {
	case scopeNone:
		return "none"
	case scopeGlobal:
		return "global"
	case scopeEnv:
		return "env"
	case scopeStack:
		return "stack"
	case scopeJob:
		return "job"
	case scopeDeployment:
		return "deployment"
	case scopeMonitor:
		return "monitor"
	case scopeVolumeSource:
		return "volumeSource"
	case scopeAny:
		return "any"
	case scopeBody:
		return "body"
	}
	return "unset"
}

// scopeNotes is the sentence each scope contributes to an operation's description — the
// authorization semantics a caller cannot infer from the capability name alone.
var scopeNotes = map[scopeKind]string{
	scopeGlobal: "Requires the capability granted globally — a host-scoped grant does not satisfy it.",
	scopeEnv:    "Requires the capability at the host named in the path.",
	scopeStack: "Requires the capability at the stack's owning host. An unknown id and an " +
		"unauthorized one both answer 404 — outsiders cannot distinguish them.",
	scopeJob: "Requires the capability at the backup job's owning host. Unknown and " +
		"unauthorized ids both answer 404.",
	scopeDeployment: "Requires the capability at the deployment's stack's host. Unknown and " +
		"unauthorized ids both answer 404.",
	scopeMonitor: "Requires the capability at the host the monitor watches; a monitor watching " +
		"every host requires it globally. Unknown and unauthorized ids both answer 404.",
	scopeVolumeSource: "Requires the capability at the host the volume source delivers to. " +
		"Unknown and unauthorized ids both answer 404.",
	scopeAny:  "Satisfied by the capability held globally or on any host; list responses are filtered to what the caller may see.",
	scopeBody: "The target environment arrives in the request body; the handler checks the capability there after decoding.",
}

// tsNames overrides the generated component/interface name for Go types whose wire name
// is load-bearing in the views. Default: the bare Go type name, exported-cased.
var tsNames = map[reflect.Type]string{
	reflect.TypeOf(stackView{}):             "Stack",
	reflect.TypeOf(envLogConfigResponse{}):  "HostLogConfig",
	reflect.TypeOf(monitorConfigResponse{}): "MonitorConfig",

	// The stack detail page's shapes. The stacks-package types get the Stack prefix the
	// handwritten interfaces always had — their bare Go names (Service, Status, Warning)
	// are too generic to own the global TS namespace.
	reflect.TypeOf(stackDetailResponse{}): "StackDetail",
	reflect.TypeOf(stacks.Service{}):      "StackService",
	reflect.TypeOf(stacks.Status{}):       "StackStatus",
	reflect.TypeOf(stacks.Warning{}):      "StackWarning",

	reflect.TypeOf(envVarView{}):            "EnvVarItem",
	reflect.TypeOf(stackSecretView{}):       "StackSecretItem",
	reflect.TypeOf(autoDeployResponse{}):    "AutoDeployResult",
	reflect.TypeOf(deployStartedResponse{}): "DeployStarted",

	reflect.TypeOf(deploymentView{}): "Deployment",

	reflect.TypeOf(volumeSourceView{}):          "VolumeSource",
	reflect.TypeOf(volumeSourceSavedResponse{}): "VolumeSourceSaved",
	reflect.TypeOf(envView{}):                   "Environment",
	reflect.TypeOf(envNodeView{}):               "Node",
	reflect.TypeOf(agentView{}):                 "Agent",
	reflect.TypeOf(newAgentResponse{}):          "NewAgent",
	reflect.TypeOf(taskView{}):                  "Task",
	reflect.TypeOf(clusterNodeView{}):           "ClusterNode",
	reflect.TypeOf(dockerx.Info{}):              "DockerInfo",
	// Certificates & encryption keys, keyrings, backups, credential stores: the wire
	// names the handwritten client already gave these views.
	reflect.TypeOf(caView{}):               "CertAuthority",
	reflect.TypeOf(certView{}):             "Certificate",
	reflect.TypeOf(deliveryView{}):         "CertDelivery",
	reflect.TypeOf(keyView{}):              "EncryptionKey",
	reflect.TypeOf(createdKeyResponse{}):   "CreatedKey",
	reflect.TypeOf(keyringView{}):          "Keyring",
	reflect.TypeOf(keyringVersionView{}):   "KeyringVersion",
	reflect.TypeOf(keyringDeliveryView{}):  "KeyringDelivery",
	reflect.TypeOf(jobView{}):              "BackupJob",
	reflect.TypeOf(runView2{}):             "BackupRun",
	reflect.TypeOf(storageView{}):          "StorageTarget",
	reflect.TypeOf(registryView{}):         "RegistryItem",
	reflect.TypeOf(gitCredView{}):          "GitCredential",
	reflect.TypeOf(sshKeyView{}):           "SSHKey",
	reflect.TypeOf(sshKeyRequest{}):        "SSHKeyRequest",
	reflect.TypeOf(sshKeyCreateResponse{}): "CreatedSSHKey",
	// session, tokens, users, roles, identity providers, notifications, audit — the
	// handwritten interface names these views were written against.
	reflect.TypeOf(tokenView{}):              "APIToken",
	reflect.TypeOf(createdTokenResponse{}):   "CreatedAPIToken",
	reflect.TypeOf(capabilitiesResponse{}):   "CapabilityRegistry",
	reflect.TypeOf(capView{}):                "Capability",
	reflect.TypeOf(areaView{}):               "CapArea",
	reflect.TypeOf(userView{}):               "User",
	reflect.TypeOf(membershipView{}):         "Membership",
	reflect.TypeOf(roleView{}):               "Role",
	reflect.TypeOf(providerView{}):           "Provider",
	reflect.TypeOf(store.OIDCRoleMapping{}):  "RoleMapping",
	reflect.TypeOf(smtpView{}):               "SMTPSettings",
	reflect.TypeOf(notifyEventView{}):        "NotifyEvent",
	reflect.TypeOf(store.NotificationRule{}): "NotifyRule",
	reflect.TypeOf(channelView{}):            "NotifyChannel",
	reflect.TypeOf(failedNotificationView{}): "FailedNotification",
	reflect.TypeOf(auditEntryView{}):         "AuditEntry",
}

// RouteMeta is one route, in the generator's vocabulary. Everything here is derived from
// the table — the compiler keeps it honest, which is the entire reason the payload types
// live as struct fields rather than as comments.
type RouteMeta struct {
	Method  string
	Path    string
	Pattern string

	Cap   string // "logging.edit"; "" when open
	Scope string
	Note  string // the scope's security sentence
	Open  string // the open reason; "" when capability-guarded

	Req          reflect.Type // nil = not declared
	Resp         reflect.Type // nil = not declared
	RespNullable bool         // resp was a typed nil pointer: the handler may answer null
	TSName       string

	OperationID string
}

// RouteMetas exports the table. A zero Server is safe: the table is pure struct
// literals, and constructing a method value never dereferences its receiver —
// routes_test.go has relied on exactly this since the table existed.
func RouteMetas() ([]RouteMeta, error) {
	var out []RouteMeta
	seen := map[string]string{}
	for _, rt := range (&Server{}).apiRoutes() {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			return nil, fmt.Errorf("api: route pattern %q has no method", rt.pattern)
		}
		m := RouteMeta{
			Method:  method,
			Path:    path,
			Pattern: rt.pattern,
			Scope:   rt.scope.String(),
			Note:    scopeNotes[rt.scope],
			Open:    rt.open,
			TSName:  rt.ts,
		}
		if !rt.cap.IsZero() {
			m.Cap = rt.cap.Name()
		}
		if rt.req != nil {
			m.Req = reflect.TypeOf(rt.req)
		}
		if rt.resp != nil {
			t := reflect.TypeOf(rt.resp)
			// A typed nil pointer — (*T)(nil) — is the table's way of saying "T or null":
			// the handler answers null when the thing is unset. A pointer VALUE would say
			// the same thing by accident, so the convention is checked, not inferred.
			if t.Kind() == reflect.Pointer {
				m.RespNullable = true
				t = t.Elem()
			}
			m.Resp = t
		}

		m.OperationID = rt.ts
		if m.OperationID == "" {
			m.OperationID = operationID(method, path)
		}
		if prev, dup := seen[m.OperationID]; dup {
			return nil, fmt.Errorf("api: %s and %s derive the same operationId %q — set ts: on one",
				prev, rt.pattern, m.OperationID)
		}
		seen[m.OperationID] = rt.pattern

		out = append(out, m)
	}
	return out, nil
}

// operationID is the fallback identity for routes with no ts: name — the pattern,
// camel-cased: GET /api/clusters/{cluster}/logging → getEnvironmentsByEnvLogging.
// TSName wins when set, so the spec and the client share one identity and cannot drift.
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/api/"), "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") {
			b.WriteString("By")
			seg = strings.Trim(seg, "{}")
		}
		b.WriteString(exportCase(seg))
	}
	return b.String()
}

// exportCase upper-cases the first rune and folds kebab/underscore segments:
// "volume-sources" → "VolumeSources".
func exportCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}
	return b.String()
}

// TSNameOverrides hands the override table to the generator.
func TSNameOverrides() map[reflect.Type]string { return tsNames }
