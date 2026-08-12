package manifest

// Kind identifies one resource kind, both in apply ordering and in reports.
type Kind string

const (
	KindSSHKey          Kind = "ssh_key"
	KindRegistry        Kind = "registry"
	KindGitCredential   Kind = "git_credential"
	KindNetwork         Kind = "network"
	KindCA              Kind = "ca"
	KindCertificate     Kind = "certificate"
	KindKeyring         Kind = "keyring"
	KindCertDelivery    Kind = "cert_delivery"
	KindKeyringDelivery Kind = "keyring_delivery"
	KindGeneratedSecret Kind = "generated_secret"
	KindStack           Kind = "stack"
	KindVolumeSource    Kind = "volume_source"
)

// Order is the fixed sequence apply walks. It is a topological order over how kinds
// reference each other — credentials before the things that use them, CAs before
// certificates, certificates before deliveries, stacks before the volume sources
// that may link to one — so there is no user-facing DAG to author or to debug.
// Within a kind, document order.
var Order = []Kind{
	KindSSHKey,
	KindRegistry,
	KindGitCredential,
	KindNetwork,
	KindCA,
	KindCertificate,
	KindKeyring,
	KindCertDelivery,
	KindKeyringDelivery,
	KindGeneratedSecret, // before stacks: their slots consume the generated values
	KindStack,
	KindVolumeSource,
}
