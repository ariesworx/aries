package proto

import "time"

// Access management on a server of many projects (protocol 5, design
// §15): projects, principals and memberships, for `sfx admin`. Every op
// rides an ordinary connection, which names a project the caller may
// reach; an op that acts on a project takes Project, a project's UUID or
// its name, and acts on the connection's project when it is empty. A
// server of one project refuses them all.
//
// Who may run each is the control store's to decide; a refusal is
// forbidden, with a fix that says who can.
//
// Protocol 6 adds a principal's public SSH keys, which the daemon writes
// to a file sshd reads (design §15.2): key.add, keys and key.remove, and
// Keys on principal.add. It also has status, a read for an admin
// watching the server, added before protocol 6 shipped in a release
// (version.go).
const (
	OpProjectAdd       = "project.add"       // ProjectAddArgs → ProjectAddResult; server admins
	OpProjects         = "projects"          // AdminListArgs → ProjectsResult
	OpProjectArchive   = "project.archive"   // ProjectArgs → ProjectResult; server admins
	OpProjectActivate  = "project.activate"  // ProjectArgs → ProjectResult; server admins
	OpPrincipalAdd     = "principal.add"     // PrincipalAddArgs → PrincipalAddResult
	OpPrincipals       = "principals"        // AdminListArgs → PrincipalsResult; server admins
	OpPrincipalDisable = "principal.disable" // PrincipalArgs → PrincipalResult
	OpPrincipalEnable  = "principal.enable"  // PrincipalArgs → PrincipalResult
	OpMemberSet        = "member.set"        // MemberSetArgs → MemberSetResult
	OpMembers          = "members"           // MembersArgs → MembersResult
	OpMemberRemove     = "member.remove"     // MemberArgs → MemberRemoveResult
	OpKeyAdd           = "key.add"           // KeyAddArgs → KeyResult
	OpKeys             = "keys"              // KeysArgs → KeysResult
	OpKeyRemove        = "key.remove"        // KeyRemoveArgs → KeyResult
	OpStatus           = "status"            // StatusArgs → StatusResult; server admins, and project admins for their projects
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

// ProjectAddResult is the project created, and the Dolt database it is
// in. Resumed: the project was already being created, and this request
// finished it.
type ProjectAddResult struct {
	ProjectResult
	Database string `json:"database"`
	Resumed  bool   `json:"resumed,omitempty"`
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
// when it has one. Database and CreatedBy are for server admins.
type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Account   string `json:"account"`
	State     string `json:"state"`
	Role      string `json:"role,omitempty"`
	Database  string `json:"database,omitempty"`
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
// it to no project instead. Keys (protocol 6) are its first public keys,
// each one line as KeyAddArgs.Key is, at most MaxKeysPerAdd; the
// principal and its keys are added together or not at all.
type PrincipalAddArgs struct {
	Name       string   `json:"name"`
	Project    string   `json:"project,omitempty"`
	Unassigned bool     `json:"unassigned,omitempty"`
	Keys       []string `json:"keys,omitempty"`
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
type PrincipalResult struct {
	ID          string `json:"id"`
	Rev         int64  `json:"rev"`
	State       string `json:"state"`
	Project     string `json:"project,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	Released    int    `json:"released,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

// PrincipalAddResult is the principal added, and the fingerprints of
// the keys added with it.
type PrincipalAddResult struct {
	PrincipalResult
	Keys []string `json:"keys,omitempty"`
}

// Principal is one principal in a list.
type Principal struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
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
type MemberSetArgs struct {
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`
}

// MemberSetResult is a membership after a write: the principal as ID,
// the membership's revision, its project and role, and whether it was
// added, changed (another role) or unchanged.
//
// Projects and Keys are set when the principal was added and now
// belongs to more than one project, and the caller may see its keys:
// how many projects it is in, and the fingerprints of the keys recorded
// for it, each of which now reaches this project too.
type MemberSetResult struct {
	ID          string   `json:"id"`
	Rev         int64    `json:"rev"`
	Project     string   `json:"project"`
	ProjectName string   `json:"project_name"`
	Role        string   `json:"role"`
	Change      string   `json:"change"`
	Projects    int      `json:"projects,omitempty"`
	Keys        []string `json:"keys,omitempty"`
}

// MembersArgs lists the members of Project (empty: the connection's), as
// AdminListArgs pages them.
type MembersArgs struct {
	Project string `json:"project,omitempty"`
	AdminListArgs
}

// Member is one principal's role in a project.
type Member struct {
	Principal string    `json:"principal"`
	Role      string    `json:"role"`
	AddedBy   string    `json:"added_by"`
	AddedAt   time.Time `json:"added_at"`
}

// MembersResult lists a project's members by principal. More counts
// those after the last one listed.
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

// MemberRemoveResult is the membership removed: the principal as ID and
// the project. Warning, when set, tells the admin what the removal
// changed beyond the project: that admins of the principal's remaining
// projects can now manage its keys. Released counts the claims the
// removal ended.
type MemberRemoveResult struct {
	ID          string `json:"id"`
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
	ID               string `json:"id"`
	Principal        string `json:"principal"`
	Type             string `json:"type"`
	Comment          string `json:"comment,omitempty"`
	Change           string `json:"change,omitempty"`
	Dropped          int    `json:"dropped,omitempty"`
	InAuthorizedKeys bool   `json:"in_authorized_keys,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// KeysArgs lists the keys of the principal Name.
type KeysArgs struct {
	Name string `json:"name"`
}

// Key is one public key in a list: its fingerprint, type and comment.
// InAuthorizedKeys says the key is also a line of the authorized_keys
// file the server's operator maintains by hand. sshd reads the server's
// keys file first, so the key signs in by the store's line while the
// store holds it; removing it from the store closes its connections
// and does not keep it out, since sshd then takes the other line.
type Key struct {
	Fingerprint      string    `json:"fingerprint"`
	Type             string    `json:"type"`
	Comment          string    `json:"comment,omitempty"`
	AddedBy          string    `json:"added_by"`
	AddedAt          time.Time `json:"added_at"`
	InAuthorizedKeys bool      `json:"in_authorized_keys,omitempty"`
}

// KeysResult lists a principal's keys, oldest first. FileConns counts
// the principal's open connections that named no key of the store: sshd
// let them in by a line of the hand-maintained authorized_keys file,
// which removing a key from the store does not close or prevent. Such a
// line may hold a key that is not listed here.
type KeysResult struct {
	Principal string `json:"principal"`
	Keys      []Key  `json:"keys"`
	FileConns int    `json:"file_conns,omitempty"`
}

// KeyRemoveArgs names the key to remove by its SHA-256 fingerprint, as
// `sfx admin key list` and `ssh-keygen -lf FILE.pub` print it.
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
// last one listed. Dolt is another process, so nothing here is its
// memory or its disk.
type StatusResult struct {
	Server   *ServerStatus   `json:"server,omitempty"`
	Projects []ProjectStatus `json:"projects"`
	More     int             `json:"more,omitempty"`
}

// ServerStatus is the daemon as a whole. Each count stands beside the
// limit that caps it. HeapBytes is the memory starfixd's own heap holds
// in live and not yet collected objects, and SysBytes all the memory it
// has from the operating system (Go's runtime.MemStats HeapAlloc and
// Sys). Projects and Principals count the control store's rows by state.
// KeysFile is the file of keys the daemon writes for sshd.
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
	KeysFile      *KeysFile      `json:"keys_file,omitempty"`
}

// KeysFile is the authorized_keys-format file the daemon writes from
// the control store, which sshd reads: its path on the server, how many
// keys the daemon last put in it, and when the file was last written.
// Failed says the latest attempt to write it failed, so that sshd may
// be reading an older list than the store holds; the daemon tries again
// every half minute, and its log says why.
type KeysFile struct {
	Path      string     `json:"path"`
	Keys      int        `json:"keys"`
	WrittenAt *time.Time `json:"written_at,omitempty"`
	Failed    bool       `json:"failed,omitempty"`
}

// ProjectStatus is one project. Open says whether its store is open;
// the daemon opens none to answer. Conns counts the connections to it
// past their handshake, and FileConns those among them that named no key
// of the store, which sshd let in by the hand-maintained authorized_keys
// file.
//
// The rest is set only while the store is open, and starts afresh each
// time it opens: Sessions, how many sessions were seen in the last five
// minutes, as `sfx who` lists them; Writes; and LastUsed, when the store
// last answered a request, or was opened if it has answered none.
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
