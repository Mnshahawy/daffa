// Package notify decides who is told when something happens, renders the mail, and hands it
// to the outbox.
//
// It does not send. Sending is the worker's job, outside any transaction — see outbox.go.
package notify

// Event is something worth telling somebody about. The set is deliberately small: an ops
// tool that emails about everything is an ops tool whose emails get filtered to a folder,
// and then the one that mattered goes unread with the rest.
type Event string

const (
	DeploySucceeded Event = "deploy.succeeded"
	DeployFailed    Event = "deploy.failed"
	BackupSucceeded Event = "backup.succeeded"
	BackupFailed    Event = "backup.failed"
	AgentOffline    Event = "agent.offline"
	BreakGlassUsed  Event = "breakglass.used"

	// Raised by a resource monitor. See docs/monitoring.md.
	MonitorFired    Event = "monitor.fired"
	MonitorResolved Event = "monitor.resolved"

	// Raised by the certificate manager. See docs/certs.md.
	CertExpiring    Event = "cert.expiring"
	CertRenewFailed Event = "cert.renew_failed"
	CertRenewed     Event = "cert.renewed"
	CARotationDue   Event = "ca.rotation_due"

	// Raised by the keyring rotation worker. See docs/keyrings.md.
	KeyringRotated      Event = "keyring.rotated"
	KeyringRotateFailed Event = "keyring.rotate_failed"
)

// Def describes an event for the settings page.
type Def struct {
	Event       Event
	Label       string
	Description string
	// Noisy events are off unless somebody asks for them. A deploy that worked is not news;
	// a deploy that failed at 2am is.
	Noisy bool
}

// All is the catalogue, in display order.
var All = []Def{
	{DeployFailed, "Deploy failed", "A stack deploy exited non-zero — including one a webhook started.", false},
	{BackupFailed, "Backup failed", "A backup run failed. This is the one you want.", false},
	{AgentOffline, "Host offline", "A host stopped answering.", false},
	{MonitorFired, "Resource monitor fired", "A container crossed a threshold and stayed across it — memory above 70% for ten minutes, or whatever the rule says.", false},
	{BreakGlassUsed, "Break-glass used", "Somebody redeemed a break-glass token and signed in as an administrator.", false},
	// A recovery is genuinely useful — it is how you find out at 07:00 that the thing which
	// woke you at 03:00 sorted itself out — but it is only worth having if you asked for the
	// alert in the first place, so it is not on by default either.
	{CertExpiring, "Certificate expiring", "A certificate Daffa cannot renew itself — an uploaded one — is inside its renewal window. Somebody has to bring a new one.", false},
	{CertRenewFailed, "Certificate renewal failed", "Daffa tried to re-sign a certificate and could not. It escalates as expiry approaches.", false},
	{CARotationDue, "CA rotation needs attention", "A certificate authority is approaching expiry, or a staged rotation is waiting on somebody to activate it.", false},
	{KeyringRotateFailed, "Keyring rotation failed", "A scheduled keyring rotation could not run, or the new version could not reach a delivery volume. Consumers keep the previous key meanwhile.", false},
	{MonitorResolved, "Resource monitor recovered", "A container that had crossed a threshold came back under it.", true},
	{DeploySucceeded, "Deploy succeeded", "Every successful deploy. Chatty.", true},
	{BackupSucceeded, "Backup succeeded", "Every successful backup. Chatty — the failures are what matter.", true},
	{CertRenewed, "Certificate renewed", "Every successful automatic renewal. Chatty — the failures are what matter.", true},
	{KeyringRotated, "Keyring rotated", "Every scheduled keyring rotation. Chatty — the failures are what matter.", true},
}

func (e Event) Valid() bool {
	for _, d := range All {
		if d.Event == e {
			return true
		}
	}
	return false
}

// Data is what a template renders from. It is deliberately flat and stringly-typed: a
// template that reaches into a domain object couples the mail to the schema, and then a
// refactor three months from now silently produces an email that says nothing.
type Data struct {
	Event   Event
	Subject string

	// What happened, and where.
	Title    string
	Summary  string
	HostName string
	Target   string // the stack, the job, the host

	// Detail is the interesting part: a failure's error text, a run's exit code. It is
	// rendered in a monospace block, so it may be multi-line.
	Detail string

	// Link back into the console. Empty if we cannot work out the base URL, in which case
	// the template omits the button rather than rendering a broken one.
	Link string

	Failed bool // colours the header; a red banner for a success would be a lie
}
