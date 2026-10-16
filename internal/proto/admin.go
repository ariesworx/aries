package proto

import "time"

// Access management on a server of many projects (design §15):
// projects, principals and memberships, for `aries admin`. Every op
// rides an ordinary connection, which names a project the caller may
// reach; an op that acts on a project takes Project, a project's UUID or
// its name, and acts on the connection's project when it is empty. A
// server of one project refuses them all.
//
// Who may run each is the control store's to decide; a refusal is
// forbidden, with a fix that says who can.
//
// The protocol also has a principal's public SSH keys, which the daemon
// writes to a file sshd reads (design §15.2): key.add, keys and
// key.remove, and Keys on principal.add; and status, a read for an admin
// watching the server; and principal.whois, which resolves the stand-in
// a trace carries in place of a principal's name (design §16.4).
const (
	OpProjectAdd       = "project.add"       // ProjectAddArgs → ProjectAddResult; server admins
	OpProjects         = "projects"          // AdminListArgs → ProjectsResult
	OpProjectArchive   = "project.archive"   // ProjectArgs → ProjectResult; server admins
	OpProjectActivate  = "project.activate"  // ProjectArgs → ProjectResult; server admins
	OpPrincipalAdd     = "principal.add"     // PrincipalAddArgs → PrincipalAddResult
	OpPrincipals       = "principals"        // AdminListArgs → PrincipalsResult; server admins
	OpPrincipalDisable = "principal.disable" // PrincipalArgs → PrincipalResult
	OpPrincipalEnable  = "principal.enable"  // PrincipalArgs → PrincipalResult
	OpPrincipalWhois   = "principal.whois"   // PrincipalWhoisArgs → PrincipalWhoisResult; server admins
	OpMemberSet        = "member.set"        // MemberSetArgs → MemberSetResult
	OpMembers          = "members"           // MembersArgs → MembersResult
	OpMemberRemove     = "member.remove"     // MemberArgs → MemberRemoveResult
	OpKeyAdd           = "key.add"           // KeyAddArgs → KeyResult
	OpKeys             = "keys"              // KeysArgs → KeysResult
	OpKeyRemove        = "key.remove"        // KeyRemoveArgs → KeyResult
	OpStatus           = "status"            // StatusArgs → StatusResult; server admins, and project admins for their projects

	// A principal's kind, owner and display name, and the identities a
	// person signs in with and the invitations to them
	// (docs/design/identity.md §6.5).
	OpPrincipalSet   = "principal.set"   // PrincipalSetArgs → PrincipalResult; server admins
	OpPrincipalShow  = "principal.show"  // PrincipalArgs → PrincipalShowResult; server admins, and the principal
	OpInviteAdd      = "invite.add"      // InviteAddArgs → IdentityResult; server admins
	OpInvites        = "invites"         // InvitesArgs → InvitesResult; server admins
	OpInviteRemove   = "invite.remove"   // IdentityArgs → IdentityResult; server admins
	OpIdentities     = "identities"      // IdentitiesArgs → IdentitiesResult; server admins, and the principal
	OpIdentityRemove = "identity.remove" // IdentityArgs → IdentityResult; server admins
)

// Bounds on what the admin lists return.
const (
	// AdminListDefault is how many rows an admin list returns when the
	// request gives no limit, and AdminListMax the most it may ask for.
	AdminListDefault = 200
	AdminListMax     = 1000
)

// ProjectAddArgs creates a project named Name, a code name unique on the
// server. Prefix starts its issue ids and defaults to Name; Account is
// what its issues report against by default, and defaults to the
// server's. Sent again for a project still being created, it resumes
// the creation, and Prefix and Account may then be left out.
type ProjectAddArgs struct {
	Name    string `json:"name"`
	Prefix  string `json:"prefix,omitempty"`
	Account string `json:"account,omitempty"`
}

// ProjectResult is a project after a write: its UUID, revision, name and
// state. Released counts the claims an archive ended at once, in the
// project's open store.
type ProjectResult struct {
	ID       string `json:"id"`
	Rev      int64  `json:"rev"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Released int    `json:"released,omitempty"`
}

// ProjectAddResult is the project created, and Schema, the schema the
// server made for it in its database. Resumed: the project was already
// being created, and this request finished it.
type ProjectAddResult struct {
	ProjectResult
	Schema  string `json:"schema"`
	Resumed bool   `json:"resumed,omitempty"`
}

// ProjectArgs names a project to archive or activate.
type ProjectArgs struct {
	Project string `json:"project"`
}

// AdminListArgs page an admin list, which is ordered by name: at most
// Limit rows (0 takes AdminListDefault; at most AdminListMax) whose name
// sorts after After.
type AdminListArgs struct {
	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
}

// Project is one project in a list. Role is the caller's role in it,
// when it has one. Schema, the schema that holds it, and CreatedBy are
// for server admins.
type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Account   string `json:"account"`
	State     string `json:"state"`
	Role      string `json:"role,omitempty"`
	Schema    string `json:"schema,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

// ProjectsResult lists projects by name: every one for a server admin,
// and for anyone else the active ones they belong to. More counts those
// after the last one listed.
type ProjectsResult struct {
	Projects []Project `json:"projects"`
	More     int       `json:"more,omitempty"`
}

// PrincipalAddArgs adds a principal named Name as a member of Project
// (empty: the connection's). Unassigned, for server admins only, adds
// it to no project instead. Keys are its first public keys,
// each one line as KeyAddArgs.Key is, at most MaxKeysPerAdd.
//
// Kind is person (the default) or service; a service needs Owner, a
// person it answers to. DisplayName is what people see beside the name.
// Invite, with Provider, invites that address for the principal as
// invite.add does, which only a server admin may. The principal and all
// of it are added together or not at all.
type PrincipalAddArgs struct {
	Name        string   `json:"name"`
	Project     string   `json:"project,omitempty"`
	Unassigned  bool     `json:"unassigned,omitempty"`
	Keys        []string `json:"keys,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Invite      string   `json:"invite,omitempty"`
	Provider    string   `json:"provider,omitempty"`
}

// The kinds of principal (PrincipalAddArgs.Kind, PrincipalSetArgs.Kind).
const (
	KindPerson  = "person"
	KindService = "service"
)

// PrincipalSetArgs changes the principal Name: its DisplayName when
// given (empty clears it), its Kind when given, and a service's Owner
// when given. Making a service a person clears its owner.
type PrincipalSetArgs struct {
	Name        string  `json:"name"`
	DisplayName *string `json:"display_name,omitempty"`
	Kind        string  `json:"kind,omitempty"`
	Owner       string  `json:"owner,omitempty"`
}

// MaxKeysPerAdd is the most keys one principal.add may carry. The
// server's cap on a principal's keys (keys_per_principal) also applies.
const MaxKeysPerAdd = 20

// PrincipalArgs names a principal to disable or enable.
type PrincipalArgs struct {
	Name string `json:"name"`
}

// PrincipalResult is a principal after a write: its name as ID, its
// revision and state, and for an add the project it was made a member
// of. Released counts the claims a disable ended at once, in the stores
// open then. Warning is set when the change is committed and the server
// could not write the keys file sshd reads, which may then be out of
// date.
//
// A disable also says what the principal left behind, among what the
// caller may list: KeysAdded, the keys it added that are still in the
// store, PrincipalsCreated and MembersAdded. Disabling ends none of
// them, and Warning then names the commands that list them.
//
// Kind, Owner and DisplayName are the principal's; Change, for a set, is
// "changed" or "unchanged".
type PrincipalResult struct {
	Name              string `json:"name"`
	Rev               int64  `json:"rev"`
	State             string `json:"state"`
	Kind              string `json:"kind,omitempty"`
	Owner             string `json:"owner,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	Change            string `json:"change,omitempty"`
	Project           string `json:"project,omitempty"`
	ProjectName       string `json:"project_name,omitempty"`
	Released          int    `json:"released,omitempty"`
	KeysAdded         int    `json:"keys_added,omitempty"`
	PrincipalsCreated int    `json:"principals_created,omitempty"`
	MembersAdded      int    `json:"members_added,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

// PrincipalWhoisArgs asks whose stand-in StandIn is: "p_" and 16 hex
// digits, as a span of the server's traces carries it in place of a
// principal's name (`telemetry: principals: pseudonym`).
type PrincipalWhoisArgs struct {
	StandIn string `json:"stand_in"`
}

// PrincipalWhoisResult is the principal a stand-in is for, and its
// state.
type PrincipalWhoisResult struct {
	StandIn string `json:"stand_in"`
	Name    string `json:"name"`
	State   string `json:"state,omitempty"`
}

// PrincipalsArgs lists principals, as AdminListArgs pages them: all of
// them, for server admins, or with CreatedBy those that principal
// created, which a project admin may also ask for, and is shown the ones
// that belong to a project they administer.
type PrincipalsArgs struct {
	CreatedBy string `json:"created_by,omitempty"`
	AdminListArgs
}

// PrincipalAddResult is the principal added, the fingerprints of the
// keys added with it, and the invitation made with it.
type PrincipalAddResult struct {
	PrincipalResult
	Keys       []string  `json:"keys,omitempty"`
	Invitation *Identity `json:"invitation,omitempty"`
}

// Principal is one principal in a list.
type Principal struct {
	Name        string    `json:"name"`
	State       string    `json:"state"`
	Kind        string    `json:"kind,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// PrincipalShowResult is one principal, with its identities and its
// keys, oldest first.
type PrincipalShowResult struct {
	Principal
	Identities []Identity `json:"identities"`
	Keys       []Key      `json:"keys"`
}

// The states of an identity (Identity.State).
const (
	IdentityBound   = "bound"   // a sign-in bound it
	IdentityPending = "pending" // an invitation, open until ExpiresAt
	IdentityLapsed  = "lapsed"  // an invitation that ExpiresAt passed unused
)

// Identity is an external identity of a person: an address invited at
// a provider (Address, the invitation), and, once a sign-in binds it,
// the issuer the provider's token named. The subject is never sent.
// ExpiresAt is when an invitation lapses, and is not set once bound.
type Identity struct {
	ID         int64      `json:"id"`
	Principal  string     `json:"principal"`
	Provider   string     `json:"provider"`
	Address    string     `json:"address"`
	State      string     `json:"state"`
	Issuer     string     `json:"issuer,omitempty"`
	InvitedBy  string     `json:"invited_by"`
	InvitedAt  time.Time  `json:"invited_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	BoundAt    *time.Time `json:"bound_at,omitempty"`
	LastSignIn *time.Time `json:"last_sign_in,omitempty"`
}

// InviteAddArgs invites Address at Provider for the principal Name, a
// person. An invitation of the address there for Name that no sign-in
// has bound is renewed.
type InviteAddArgs struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Address  string `json:"address"`
}

// IdentityArgs names an identity or an invitation by its id, as the
// lists print it.
type IdentityArgs struct {
	ID int64 `json:"id"`
}

// IdentityResult is an identity after a write. Change, for invite.add, is
// "added" or "renewed".
type IdentityResult struct {
	Identity
	Change string `json:"change,omitempty"`
}

// InvitesArgs lists the invitations no sign-in has bound, by id, as
// AdminListArgs pages a list, After being an id; Pending lists only
// those that have not lapsed.
type InvitesArgs struct {
	Pending bool `json:"pending,omitempty"`
	AdminListArgs
}

// InvitesResult lists invitations by id. More counts those after the
// last one listed.
type InvitesResult struct {
	Invites []Identity `json:"invites"`
	More    int        `json:"more,omitempty"`
}

// IdentitiesArgs lists the identities of the principal Name.
type IdentitiesArgs struct {
	Name string `json:"name"`
}

// IdentitiesResult lists a principal's identities, bound and not, oldest
// first.
type IdentitiesResult struct {
	Principal  string     `json:"principal"`
	Identities []Identity `json:"identities"`
}

// PrincipalsResult lists principals by name, disabled ones included.
// More counts those after the last one listed.
type PrincipalsResult struct {
	Principals []Principal `json:"principals"`
	More       int         `json:"more,omitempty"`
}

// MemberSetArgs gives the principal Name the role Role (member or admin;
// empty is member) in Project (empty: the connection's): it adds the
// membership or changes its role.
//
// A key signs in to every project its principal is in, so a new
// membership brings the principal's keys with it (design §14, decision
// 36). Without AcceptKeys, a request that would add a principal who has
// any key adds nothing, and answers with the keys (MemberSetResult's
// Change is pending); the same request with AcceptKeys adds it. A
// principal with no key is added either way, and so is a role changed.
type MemberSetArgs struct {
	Name       string `json:"name"`
	Project    string `json:"project,omitempty"`
	Role       string `json:"role,omitempty"`
	AcceptKeys bool   `json:"accept_keys,omitempty"`
}

// MemberSetResult is a membership after a write: the principal as ID,
// the membership's revision, its project and role, and whether it was
// added, changed (another role) or unchanged.
//
// Change is pending, and Rev 0, when nothing was written because the
// principal has keys the request did not accept. Carried then lists
// every key the principal has, and Role the role asked for. Carried is
// also set when the principal was added: the keys that now sign in to
// the project. Projects is how many projects the principal belongs to,
// counting this one once it is added. Every caller is shown these,
// whether or not they may change the principal's keys.
type MemberSetResult struct {
	Name        string       `json:"name"`
	Rev         int64        `json:"rev"`
	Project     string       `json:"project"`
	ProjectName string       `json:"project_name"`
	Role        string       `json:"role"`
	Change      string       `json:"change"`
	Projects    int          `json:"projects,omitempty"`
	Carried     []CarriedKey `json:"carried,omitempty"`
}

// MemberPending is MemberSetResult's Change when the principal was not
// added because its keys were not accepted.
const MemberPending = "pending"

// CarriedKey is one key of a principal joining a project: its
// fingerprint, type and comment, who added it and when.
// AdderAdministers says whether whoever added it is, now, an admin of
// the project being joined or a server admin, or was the server's
// operator: a key added by anyone else was put there by someone with no
// say over this project.
type CarriedKey struct {
	Fingerprint      string    `json:"fingerprint"`
	Type             string    `json:"type"`
	Comment          string    `json:"comment,omitempty"`
	AddedBy          string    `json:"added_by"`
	AddedAt          time.Time `json:"added_at"`
	AdderAdministers bool      `json:"adder_administers,omitempty"`
}

// MembersArgs lists the members of Project (empty: the connection's), as
// AdminListArgs pages them.
//
// With AddedBy it lists instead the memberships that principal added:
// in Project when one is named, and otherwise in every project the
// caller may list the members of, each row naming its project. Those
// rows are by project name and then principal, and After is then the
// last row's PROJECT/PRINCIPAL.
type MembersArgs struct {
	Project string `json:"project,omitempty"`
	AddedBy string `json:"added_by,omitempty"`
	AdminListArgs
}

// Member is one principal's role in a project. Project and ProjectName
// are set in a list that spans projects.
type Member struct {
	Principal   string    `json:"principal"`
	Role        string    `json:"role"`
	AddedBy     string    `json:"added_by"`
	AddedAt     time.Time `json:"added_at"`
	Project     string    `json:"project,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
}

// MembersResult lists a project's members by principal. More counts
// those after the last one listed. Project and ProjectName are empty for
// a list that spans projects.
type MembersResult struct {
	Project     string   `json:"project"`
	ProjectName string   `json:"project_name"`
	Members     []Member `json:"members"`
	More        int      `json:"more,omitempty"`
}

// MemberArgs names the principal Name's membership of Project (empty:
// the connection's), to remove.
type MemberArgs struct {
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
}

// MemberRemoveResult is the membership removed: the principal as Name
// and the project. Warning, when set, tells the admin what the removal
// changed beyond the project: that admins of the principal's remaining
// projects can now manage its keys. Released counts the claims the
// removal ended.
type MemberRemoveResult struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	ProjectName string `json:"project_name"`
	Warning     string `json:"warning,omitempty"`
	Released    int    `json:"released,omitempty"`
}

// KeyAddArgs adds a public SSH key to the principal Name. Key is one
// line in authorized_keys form, TYPE BASE64 [COMMENT], with no options:
// what a .pub file holds. Only public keys are ever sent.
type KeyAddArgs struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// KeyResult is a key after a write: its SHA-256 fingerprint as ID, the
// principal it belongs to, its type and comment, and for an add whether
// it was added or the principal had it already (unchanged). Dropped
// counts the connections a removal closed: those the key had
// authenticated.
//
// InAuthorizedKeys is set by a removal when the key is also a line of
// the authorized_keys file the server's operator maintains by hand:
// its connections were closed, and sshd will still accept it by that
// line until the line is deleted. Warning then says so, and what to do. It
// also says when the change is committed and the server could not write
// the keys file sshd reads, which may then be out of date.
type KeyResult struct {
	Fingerprint      string `json:"fingerprint"`
	Principal        string `json:"principal"`
	Type             string `json:"type"`
	Comment          string `json:"comment,omitempty"`
	Change           string `json:"change,omitempty"`
	Dropped          int    `json:"dropped,omitempty"`
	InAuthorizedKeys bool   `json:"in_authorized_keys,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// KeysArgs lists the keys of the principal Name. With AddedBy, and no
// Name, it lists instead the keys that principal added, whoever each
// belongs to, among those the caller may list, by fingerprint and paged
// as AdminListArgs pages a list.
type KeysArgs struct {
	Name    string `json:"name,omitempty"`
	AddedBy string `json:"added_by,omitempty"`
	AdminListArgs
}

// Key is one public key in a list: its fingerprint, type and comment.
// InAuthorizedKeys says the key is also a line of the authorized_keys
// file the server's operator maintains by hand. sshd reads the server's
// keys file first, so the key signs in by the store's line while the
// store holds it; removing it from the store closes its connections
// and does not keep it out, since sshd then takes the other line.
// Principal is whose key it is, in a list of the keys someone added.
type Key struct {
	Fingerprint      string    `json:"fingerprint"`
	Type             string    `json:"type"`
	Comment          string    `json:"comment,omitempty"`
	AddedBy          string    `json:"added_by"`
	AddedAt          time.Time `json:"added_at"`
	InAuthorizedKeys bool      `json:"in_authorized_keys,omitempty"`
	Principal        string    `json:"principal,omitempty"`
}

// KeysResult lists a principal's keys, oldest first. FileConns counts
// the principal's open connections that named no key of the store: sshd
// let them in by a line of the hand-maintained authorized_keys file,
// which removing a key from the store does not close or prevent. Such a
// line may hold a key that is not listed here.
//
// For a list of the keys someone added, Principal is empty, AddedBy
// names who, and More counts the keys after the last one listed.
type KeysResult struct {
	Principal string `json:"principal,omitempty"`
	AddedBy   string `json:"added_by,omitempty"`
	Keys      []Key  `json:"keys"`
	FileConns int    `json:"file_conns,omitempty"`
	More      int    `json:"more,omitempty"`
}

// KeyRemoveArgs names the key to remove by its SHA-256 fingerprint, as
// `aries admin key list` and `ssh-keygen -lf FILE.pub` print it.
type KeyRemoveArgs struct {
	Fingerprint string `json:"fingerprint"`
}

// StatusArgs page the projects of a status report, as AdminListArgs
// pages a list.
type StatusArgs struct {
	AdminListArgs
}

// StatusResult is what the daemon knows of itself and of its projects
// now, from memory: for a server admin, Server and every project, in
// every state; for a project admin, the active projects they administer
// and no Server. Projects are by name, and More counts those after the
// last one listed. The database is another process: what it says of
// itself is Server.Postgres, and each project's size on disk
// ProjectStatus.SchemaBytes.
type StatusResult struct {
	Server   *ServerStatus   `json:"server,omitempty"`
	Projects []ProjectStatus `json:"projects"`
	More     int             `json:"more,omitempty"`
}

// ServerStatus is the daemon as a whole. Each count stands beside the
// limit that caps it. HeapBytes is the memory ariesd's own heap holds
// in live and not yet collected objects, and SysBytes all the memory it
// has from the operating system (Go's runtime.MemStats HeapAlloc and
// Sys). Projects and Principals count the control store's rows by state,
// and Identities its identities: bound, and invitations pending or
// lapsed. KeysFile is the file of keys the daemon writes for sshd.
type ServerStatus struct {
	Version       string         `json:"version"`
	ProtoMin      int            `json:"proto_min"`
	ProtoMax      int            `json:"proto_max"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	OpenStores    int            `json:"open_stores"`
	OpenStoresMax int            `json:"open_stores_max"`
	Conns         int            `json:"conns"`
	ConnsMax      int            `json:"conns_max"`
	HeapBytes     uint64         `json:"heap_bytes"`
	SysBytes      uint64         `json:"sys_bytes"`
	Goroutines    int            `json:"goroutines"`
	Projects      map[string]int `json:"projects"`
	Principals    map[string]int `json:"principals"`
	Identities    map[string]int `json:"identities,omitempty"`
	KeysFile      *KeysFile      `json:"keys_file,omitempty"`
	Clock         *ClockStatus   `json:"clock,omitempty"`
	// Traces is how people appear in the traces the server exports;
	// nil when it exports none.
	Traces *TracesStatus `json:"traces,omitempty"`
	// Postgres is the database server's own figures; nil from a server
	// from before them.
	Postgres *PostgresStatus `json:"postgres,omitempty"`
	// Transports is which transports the server serves; nil from a
	// server from before them.
	Transports *Transports `json:"transports,omitempty"`
	// SignIn is the server's sign-in at providers; nil when it signs no
	// one in.
	SignIn *SignInStatus `json:"signin,omitempty"`
}

// SignInStatus is the server's sign-in (transport.md §8.2): how many
// sign-ins are live, and each provider, with where its keys come from.
type SignInStatus struct {
	Live      int              `json:"live"`
	Providers []ProviderStatus `json:"providers"`
}

// ProviderStatus is one provider: its name, kind and issuer; whether its
// endpoints are discovered (fetch) or written out (static); where its
// keys come from (fetch, or file, with nothing fetched), their ids, and
// when they were last read; and why discovery or the keys last failed.
type ProviderStatus struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Issuer     string     `json:"issuer"`
	Discovery  string     `json:"discovery"`
	Discovered bool       `json:"discovered"`
	Keys       string     `json:"keys"`
	KeyIDs     []string   `json:"key_ids,omitempty"`
	KeysReadAt *time.Time `json:"keys_read_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Transports is which transports the daemon serves (transport.md
// §11.1): HTTPS when HTTPS is set, and SSH on or off.
type Transports struct {
	HTTPS *HTTPSStatus `json:"https,omitempty"`
	SSH   SSHStatus    `json:"ssh"`
}

// HTTPSStatus is the HTTPS listener: the address it listens on, its TLS
// mode (acme, files or proxy), the server's URLs, when the certificate
// served ends when that is known, and how many connections past their
// handshake came over it.
type HTTPSStatus struct {
	Addr         string     `json:"addr"`
	Mode         string     `json:"mode"`
	URLs         []string   `json:"urls"`
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
	Conns        int        `json:"conns"`
}

// SSHStatus is SSH: whether the daemon serves the bridge, and how many
// connections past their handshake came through it. The keys file is
// ServerStatus.KeysFile.
type SSHStatus struct {
	On    bool `json:"on"`
	Conns int  `json:"conns"`
}

// PostgresStatus is what Postgres says of itself and of the database the
// daemon's schemas are in, read with the daemon's own role, and the
// daemon's pool of connections to it, from memory.
//
// Conns counts the client sessions on the Postgres server, of every
// database and role, against ConnsMax, its max_connections;
// DatabaseConns those in ariesd's database. PoolConns is how many
// connections ariesd's pool holds, PoolInUse how many of them a request
// is using, PoolMax the most it may hold (db_conns_per_server), and
// PoolWaits how often a request waited for one, since the daemon
// started. Commits, Rollbacks, Deadlocks, Conflicts (queries a standby
// canceled) and TempBytes (written to temporary files) count since
// Postgres last reset its statistics. DatabaseBytes is the database's
// size on disk.
//
// When Postgres could not be read, Unknown says why in a few words and
// only the pool's figures are set.
type PostgresStatus struct {
	Version       string `json:"version,omitempty"`
	Conns         int    `json:"conns"`
	ConnsMax      int    `json:"conns_max"`
	DatabaseConns int    `json:"database_conns"`
	PoolConns     int    `json:"pool_conns"`
	PoolInUse     int    `json:"pool_in_use"`
	PoolMax       int    `json:"pool_max"`
	PoolWaits     int64  `json:"pool_waits"`
	Commits       int64  `json:"commits"`
	Rollbacks     int64  `json:"rollbacks"`
	Deadlocks     int64  `json:"deadlocks"`
	Conflicts     int64  `json:"conflicts"`
	TempBytes     int64  `json:"temp_bytes"`
	DatabaseBytes int64  `json:"database_bytes"`
	Unknown       string `json:"unknown,omitempty"`
}

// TracesStatus is how people appear in the traces the server exports;
// a server that exports none has none. People is "pseudonym" (a stable
// stand-in for each name), "name" or "omit", as `telemetry: principals:`
// has it, or "unavailable": the setting is pseudonym, and spans say
// nothing about who because the key the stand-ins are made with cannot
// be read or made. Cause then says why, KeyFile names the key's file,
// and Damaged says the file is there and cannot be trusted (others can
// read it, or it does not hold a key).
type TracesStatus struct {
	People  string `json:"people"`
	Cause   string `json:"cause,omitempty"`
	Damaged bool   `json:"damaged,omitempty"`
	KeyFile string `json:"key_file,omitempty"`
}

// ClockStatus is the server's clock as the daemon has seen it since it
// started. Now is the clock's reading. StepsForward and StepsBackward
// count the times it was set, or the machine suspended: a difference
// between elapsed wall-clock and monotonic time past the server's
// tolerance. LastStepAt is the clock's time just after the latest, and
// LastStepSeconds its size, negative for a step back. Behind is set
// while the clock is behind the history of a database the server
// serves.
type ClockStatus struct {
	Now             time.Time    `json:"now"`
	StepsForward    int          `json:"steps_forward"`
	StepsBackward   int          `json:"steps_backward"`
	LastStepAt      *time.Time   `json:"last_step_at,omitempty"`
	LastStepSeconds float64      `json:"last_step_seconds,omitempty"`
	Behind          *ClockBehind `json:"behind,omitempty"`
}

// ClockBehind says the server's clock is behind its own history: by
// Seconds, as the daemon last saw it, since Since, against the latest
// event of Log, the database furthest ahead, which is at History. The
// server's time runs on from the history at half speed until the clock
// has caught up. RefusesStart says the daemon would refuse to start as
// things are (clock_behind: stop).
type ClockBehind struct {
	Seconds      int64     `json:"seconds"`
	Since        time.Time `json:"since"`
	Log          string    `json:"log"`
	History      time.Time `json:"history"`
	RefusesStart bool      `json:"refuses_start,omitempty"`
}

// KeysFile is the authorized_keys-format file the daemon writes from
// the control store, which sshd reads: its path on the server, how many
// keys the daemon last put in it, and when the file was last written.
// Failed says the latest attempt to write it failed, so that sshd may
// be reading an older list than the store holds; the daemon tries again
// every half minute, and its log says why. HeldBack counts the store's
// keys left out of the file because the server's hand-maintained
// authorized_keys file gives them to another principal: sshd reads the
// daemon's file first, and that line must not be shadowed.
type KeysFile struct {
	Path      string     `json:"path"`
	Keys      int        `json:"keys"`
	WrittenAt *time.Time `json:"written_at,omitempty"`
	Failed    bool       `json:"failed,omitempty"`
	HeldBack  int        `json:"held_back,omitempty"`
}

// ProjectStatus is one project. Open says whether its store is open;
// the daemon opens none to answer. Conns counts the connections to it
// past their handshake, and FileConns those among them that named no key
// of the store, which sshd let in by the hand-maintained authorized_keys
// file.
//
// The rest is set only while the store is open, and starts afresh each
// time it opens: Sessions, how many sessions were seen in the last five
// minutes, as `aries who` lists them; Writes; and LastUsed, when the store
// last answered a request, or was opened if it has answered none.
//
// Sessions is the one figure read from the database, within a bound.
// When it could not be read it is left out, never given as zero, and
// SessionsUnknown says why in a few words, such as "database did not
// answer in 2s". A server from before the field leaves both out.
type ProjectStatus struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	State     string       `json:"state"`
	Open      bool         `json:"open"`
	Conns     int          `json:"conns"`
	FileConns int          `json:"file_conns,omitempty"`
	Sessions  *int         `json:"sessions,omitempty"`
	Writes    *WriteStatus `json:"writes,omitempty"`
	LastUsed  *time.Time   `json:"last_used,omitempty"`

	SessionsUnknown string `json:"sessions_unknown,omitempty"`
	// SchemaBytes is the size on disk of the project's schema, open or
	// not; left out when it could not be read, or by a server from
	// before it.
	SchemaBytes *int64 `json:"schema_bytes,omitempty"`
}

// WriteStatus is how busy a project's one writer is. A write is a
// transaction that changed a row and committed; a refused request is not
// one. LastMinute counts the writes of the last 60 seconds. P50Micros
// and P99Micros are the median and the 99th percentile, by nearest rank,
// of the latency of the latest Samples writes (at most 256, however old),
// in microseconds: from the request reaching the store to its commit,
// the wait for the writer and any retry included.
type WriteStatus struct {
	LastMinute int   `json:"last_minute"`
	Samples    int   `json:"samples"`
	P50Micros  int64 `json:"p50_us"`
	P99Micros  int64 `json:"p99_us"`
}
