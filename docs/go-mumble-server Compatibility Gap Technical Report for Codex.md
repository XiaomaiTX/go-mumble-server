# go-mumble-server Compatibility Gap Technical Report

## 1. Purpose

This report documents the currently identified compatibility, correctness, security, and behavioral gaps in:

`github.com/dchote/go-mumble-server`

Target comparison baseline:

- Mumble / Murmur 1.5.915
- Mumble TCP control protocol
- Mumble legacy UDP audio protocol
- Mumble 1.5+ protobuf UDP protocol
- Murmur ACL and Group semantics
- Murmur runtime behavior

The purpose is not merely to make the server “connectable by Mumble clients”, but to make it suitable as a reliable backend for a Gin + Vue management platform.

The intended deployment model is:

```text
Vue
 ↓
Gin
 ↓ private management API
go-mumble-server
 ↕
Mumble clients
```

The native Mumble client is primarily considered a voice client. Administrative operations may be moved to the Web platform.

---

# 2. Executive Technical Assessment

Current state:

```text
Basic authentication        usable
Basic channel voice         mostly usable
REST administration         usable
Mute/deafen/move            mostly implemented
Registered users            implemented via REST
Channel CRUD                implemented
ACL storage                 implemented
ACL evaluation              behaviorally incompatible in several cases
Whisper/VoiceTarget         unsafe / incomplete
Linked-channel voice        behaviorally incompatible
Native ACL editor           incomplete
Native UserList             unimplemented
Mumble 1.5 protobuf UDP     not actually implemented
Channel listeners           unimplemented
Plugin data relay           unimplemented
Multi virtual server        metadata exists, runtime effectively server 1 only
```

The current implementation should not be considered a full drop-in Murmur replacement.

The most important defects affect:

1. VoiceTarget / whisper correctness and confidentiality
2. ACL evaluation correctness
3. Linked-channel voice authorization
4. BanList authorization
5. TextMessage subtree authorization
6. Temporary channel semantics
7. Native management compatibility

---

# 3. Severity Definitions

```text
P0
Security, privacy, authorization bypass, or severe correctness issue.
Must be fixed before production use.

P1
Important protocol or behavioral incompatibility affecting common functionality.
Should be fixed before general production deployment.

P2
Feature incompleteness or compatibility issue with viable architectural workaround.

P3
Optional or low-priority compatibility feature.
Can be deferred if not required by the intended deployment model.
```

---

# 4. P0 — VoiceTarget Stores Resolved Session IDs

## Problem

Current implementation resolves a `VoiceTarget` into recipient session IDs when the control message is received and caches the result approximately as:

```go
voiceTargets[sourceSession][targetID] = []uint32{recipientSessionIDs...}
```

The server therefore stores resolved recipients rather than the original semantic VoiceTarget specification.

Murmur instead retains target semantics such as:

```text
sessions
channel
children
links
group
```

and evaluates the target against current runtime state.

## Critical stale-session scenario

Example:

```text
User A defines VoiceTarget #4 to User B.

B session = 17

voiceTargets[A][4] = [17]
```

B disconnects.

Current cleanup deletes:

```go
voiceTargets.Delete(BSession)
```

This removes targets owned by B.

It does not remove B's session ID from targets owned by A.

The session allocator later reuses freed IDs.

New user C may receive:

```text
session = 17
```

A activates VoiceTarget #4.

Current cached target still contains:

```text
[17]
```

Audio can therefore be routed to C instead of B.

## Risk

Potential unintended private voice disclosure.

This is a confidentiality issue and should be treated as P0.

## Required redesign

Do not persist resolved recipients as the authoritative VoiceTarget representation.

Introduce semantic structures similar to:

```go
type VoiceTargetSpec struct {
    ID       uint32
    Sessions []uint32
    Channels []VoiceTargetChannel
}

type VoiceTargetChannel struct {
    ChannelID uint32
    Group     string
    Links     bool
    Children  bool
}
```

Store:

```text
source session
    ↓
VoiceTarget specification
```

Resolve recipients from current state when audio begins, or maintain a properly invalidated cache.

Required invalidation events:

```text
user connect
user disconnect
session reuse
user channel move
channel creation/deletion
channel link changes
ACL changes
group membership changes
access token changes
listener changes
```

At minimum, direct user session targets must be validated against current connection identity.

---

# 5. P0 — Whisper Authorization Path Is Incomplete

## Problem

Normal channel voice uses a path equivalent to:

```text
CanSenderSpeak()
FilterRecipient()
```

and checks:

```text
mute
suppress
self mute
Speak ACL
recipient deaf
recipient self-deaf
```

VoiceTarget IDs 1–30 take a separate path that directly obtains cached recipients.

This path does not visibly perform equivalent authorization checks.

## Missing or incomplete checks

Potentially missing:

```text
PermissionWhisper
sender mute/suppress/self-mute
per-target channel permission
recipient deaf/self-deaf filtering
dynamic group membership
dynamic channel membership
```

## Required behavior

Murmur-style voice target resolution must distinguish:

```text
direct user whisper
channel whisper
channel + children
channel + links
channel + group
```

For channel whisper, authorization must be checked against the target channel.

Do not assume `Speak` in the sender's current channel implies `Whisper` to another channel.

---

# 6. P0/P1 — ACL Evaluation Is Not Murmur-Compatible

## Current algorithm

Current evaluator approximately performs:

```text
start with default permissions

for ACL entry:
    granted |= allow
    granted &= ~deny
```

`Write` is treated specially in `Check()` as effectively granting arbitrary permission.

## Murmur behavior is more complex

Murmur maintains separate state for:

```text
traverse
write
effective permissions
```

and applies special semantics for:

```text
Traverse
Write
root-only global permissions
ACL inheritance
ApplyHere
ApplySubs
```

## Major differences

### 6.1 Traverse propagation

A Traverse denial on an intermediate channel may block access to deeper descendants even when normal ACL inheritance does not apply.

Example:

```text
Root
└── A
    └── B
        └── C

A:
Deny Traverse
ApplyHere=true
ApplySubs=false
```

Murmur still prevents traversal through A to reach C.

A normal bitmask-only model does not correctly represent this.

### 6.2 Write semantics

Murmur does not treat child-channel Write as universal server administration.

Channel-level Write implies a specific set of channel privileges.

Only root-level Write expands into server-global privileges such as:

```text
Kick
Ban
Register
SelfRegister
ResetUserContent
```

Current implementation risks treating Write too broadly.

### 6.3 Root-only permissions

The following should only be globally granted from Root:

```text
Kick
Ban
Register
SelfRegister
ResetUserContent
```

Current evaluator does not fully enforce the same root-only semantics.

## Required action

Reimplement ACL evaluation based directly on Murmur's `ACL.cpp` semantics rather than maintaining a simplified approximation.

Recommended approach:

```text
Port algorithm structurally
not behaviorally approximate it
```

Add golden tests comparing expected permission masks with Murmur test cases.

---

# 7. P0/P1 — Group Evaluation Is Not Murmur-Compatible

## Supported subset

Current implementation supports approximately:

```text
all
auth
in
out
sub
admin
stored groups
access token
invert
eval-here
```

## Missing Murmur semantics

Missing or incomplete:

```text
none
strong
$certificateHash
sub,<offset>,<min>,<max>
temporary group members
case-insensitive access tokens
```

## Access-token comparison

Murmur compares access tokens case-insensitively.

Current Go logic appears to use plain string equality.

Required fix:

```go
strings.EqualFold(a, b)
```

or equivalent normalized storage.

---

# 8. P0/P1 — Group Inheritance Order May Be Incorrect

Murmur evaluates stored group membership by collecting groups through the hierarchy and applying parent state before child state.

Expected behavior:

```text
Root:
add user 10

Child:
remove user 10
```

Result:

```text
user 10 is NOT a member
```

Current implementation walks an ancestor chain that is ordered:

```text
target → parent → root
```

and appears to apply add/remove operations in that same order.

This can reverse precedence:

```text
Child remove
then Root add
```

resulting in the user being incorrectly restored to the group.

## Required action

Match Murmur's stack behavior:

```text
collect current → root
apply root → current
```

Correctly implement:

```text
inherit
inheritable
```

as separate concepts.

---

# 9. P1 — Linked Channels Are Only Resolved One Level

Current `LinkedChannelIDs(channelID)` appears to return:

```text
current channel
+
direct links
```

Murmur's `Channel::allLinks()` traverses the full connected component.

Example:

```text
A ↔ B ↔ C
```

Expected:

```text
A.allLinks() = A, B, C
```

Current behavior may resolve only:

```text
A, B
```

## Required fix

Perform DFS/BFS over the link graph with a visited set.

---

# 10. P1 — Linked Channel Speak Permission Is Not Evaluated Per Channel

Murmur checks speaker authorization for every linked destination channel.

Example:

```text
A ↔ B

Speaker is in A

Speak(A) = allow
Speak(B) = deny
```

Expected:

```text
A receives audio
B does not
```

Current implementation appears to validate Speak against the sender's current channel once and then broadcasts to linked channel members.

## Required fix

During recipient resolution:

```go
for each linkedChannel {
    if !HasPermission(sender, linkedChannel, PermissionSpeak) {
        continue
    }
}
```

---

# 11. P1 — BanList Query Authorization

Current update path checks `PermissionBan` only when the incoming message is an update with a non-empty ban list.

A query request can therefore potentially retrieve the current BanList without the expected permission check.

Ban entries may expose:

```text
IP addresses
usernames
certificate hashes
reasons
```

## Required fix

Both:

```text
BanList query
BanList mutation
```

must require the Murmur-equivalent root Ban permission.

---

# 12. P1 — Empty BanList Cannot Clear All Bans

Current mutation logic appears conditioned on:

```go
len(msg.Bans) > 0
```

Murmur semantics allow:

```text
Query=false
Bans=[]
```

to mean replacing the list with an empty list.

## Required fix

Differentiate message intent from list length.

---

# 13. P1 — TextMessage Tree Authorization

Current tree message routing appears to:

```text
check TextMessage permission on tree root
then collect subtree recipients
```

Murmur effectively requires permission-sensitive propagation.

Example:

```text
Fleet
├── Public
└── Secret

Allow TextMessage on Fleet
Deny TextMessage on Secret
```

A tree-targeted message should not automatically bypass restrictions on Secret.

## Required fix

Evaluate destination permission per affected channel before adding recipients.

---

# 14. P1/P2 — Temporary Channels Are Persisted

Current `ChannelManager.Create()` persists channels to SQLite even when:

```text
temporary = true
```

There is no confirmed automatic deletion when the last user leaves.

This differs from Murmur temporary-channel semantics.

Expected:

```text
temporary channel
- not persistent across restart
- removed automatically when empty
- separate MakeTempChannel authorization
```

## Required fix

Either:

### Option A — real ephemeral channels

Maintain temporary channels only in memory.

or:

### Option B — persisted representation with lifecycle cleanup

Persist if necessary, but guarantee:

```text
delete when empty
delete on restart cleanup
do not restore as permanent
```

Option A is closer to Murmur.

---

# 15. P2 — ChannelState Proto2 Presence Handling

Some channel update fields are still likely interpreted via value checks.

This is incorrect for proto2 fields.

These are different:

```text
field absent
field present with zero
```

Examples:

```text
position = 0
max_users = 0
description = ""
```

A zero value may be an intentional update.

## Required action

Use explicit field-presence tracking for all proto2 mutable state fields, similar to the project's corrected UserState handling.

---

# 16. P2 — Channel Reparent Not Implemented

Murmur supports moving a channel between parents using `ChannelState.parent`.

Current channel update options contain no parent mutation.

Required support:

```text
validate target parent
prevent cycles
enforce nesting limit
check permissions
update DB
rebuild tree
broadcast ChannelState
```

---

# 17. P2 — Channel Link Permission Semantics

Murmur uses dedicated:

```text
LinkChannel
```

authorization.

Creating a link requires appropriate rights over both sides.

Current management paths may rely too heavily on generic Write behavior.

Required action:

Implement Murmur-style link authorization explicitly.

---

# 18. P2 — Native ACL Message Is Incomplete

`ACL.groups` and `ACL.acls` are not implemented in the protocol representation.

Current query behavior is effectively insufficient for the native Mumble ACL editor.

Impact:

```text
Mumble client
→ Edit Channel
→ ACL / Groups
```

is not compatible.

For the intended Web-only administration architecture, this can be deferred.

---

# 19. P2 — UserList Is a No-Op

Current handler returns without implementing registered-user management.

Impact:

Native Mumble registered-user UI is unavailable.

REST registered-user management is implemented, so this is acceptable if the Web platform is the sole management surface.

---

# 20. P2 — QueryUsers Semantics Are Incorrect

Mumble `QueryUsers` is intended for:

```text
registered user ID ↔ registered username
```

Current implementation appears to use:

```text
online session ID ↔ online username
```

This breaks protocol semantics and affects:

```text
native ACL editor
registered-user selection
administrative clients
bots relying on registered IDs
```

Required fix:

Resolve against the registered-user database, not active session state.

---

# 21. P2/P3 — UserStats Is Highly Incomplete

Missing or incomplete fields include:

```text
certificates
network statistics
UDP/TCP packet counters
ping average/variance
client version
bandwidth
online time
idle time
strong certificate
Opus
rolling stats
```

Impact is mainly the Mumble "User Information" UI and monitoring tools.

Not a core voice blocker.

---

# 22. P3 — Channel Listener Not Implemented

Mumble supports listening to channels without joining them.

Missing runtime support includes:

```text
listening_channel_add
listening_channel_remove
listening_volume_adjustment
listener recipient routing
AudioContext LISTEN
```

Current audio routing is channel-membership based.

Defer if the intended EVE deployment does not require listeners.

---

# 23. P3 — PluginDataTransmission Is a No-Op

Plugin data relay is not implemented.

Potential impact:

```text
Mumble plugins
game-specific integrations
plugin-to-plugin communication
```

Ordinary EVE voice use is unaffected unless specific Mumble plugins depend on it.

---

# 24. P3 — ContextAction Integration Is Missing

The message is accepted but meaningful server-side callback behavior is absent.

This primarily affects third-party integrations and server-side plugin logic.

---

# 25. Mumble 1.5+ Protobuf UDP Is Not Actually Implemented

This is an important documentation/code mismatch.

Project documentation states that modern protobuf UDP is implemented.

Actual audio code currently parses the legacy binary format:

```text
1 byte codec/target
Mumble varint sequence
payload length
payload
```

Mumble 1.5+ uses `MumbleUDP.Audio`, including:

```text
target/context
sender_session
frame_number
opus_data
positional_data
volume_adjustment
is_terminator
```

The current server does not appear to implement this encoding/decoding path.

## Current compatibility protection

The server currently sends a Version message without a valid 1.5 protocol version.

This likely causes modern clients to remain on legacy protocol behavior.

This accidental fallback currently protects compatibility.

## Important rule

Do not simply add:

```text
VersionV2 >= 1.5.0
```

until protobuf UDP is implemented and tested.

Doing so may cause modern clients to switch to an unsupported audio protocol.

---

# 26. Server Version Advertisement Is Incomplete

The server sends:

```text
Release
OS
OSVersion
CryptoModes
```

but apparently omits proper `VersionV1` / `VersionV2`.

This is not ideal, but currently prevents 1.5 clients from negotiating the newer UDP path.

Required future sequence:

```text
implement protobuf UDP
→ add tests
→ advertise correct protocol version
```

Not the reverse.

---

# 27. Multi Virtual Server Is Not Actually Runtime-Complete

The database and REST models expose virtual-server concepts.

However runtime startup appears hard-coded around:

```go
serverID = 1
```

with one:

```text
TCP listener
UDP listener
Mumble server instance
```

Callbacks also frequently gate on:

```go
if serverID == 1
```

Therefore current support should be described as:

```text
multi-server metadata/API scaffolding
```

not:

```text
multiple independently running Mumble virtual servers
```

This does not affect a single-server deployment.

---

# 28. Authentication Behavior Differences

Core registered-user authentication is usable.

However Murmur includes additional behaviors such as:

```text
external authenticator callbacks
re-authentication for access-token updates
ghost connection handling
last-channel restoration
more complete rejection behavior
```

The target architecture does not necessarily need to reproduce all legacy Murmur behavior.

Recommended direction:

```text
Gin = identity authority
go-mumble-server = voice/runtime authority
```

Introduce a clear authentication interface rather than copying Murmur's Ice model.

Suggested abstraction:

```go
type Authenticator interface {
    Authenticate(
        ctx context.Context,
        req AuthenticateRequest,
    ) (AuthenticateResult, error)
}
```

Possible implementations:

```text
LocalAuthenticator
GinHTTPAuthenticator
SharedDBAuthenticator
```

---

# 29. SuperUser Semantics Differ

Murmur:

```text
registered user ID 0 = SuperUser
```

go-mumble-server uses user ID 0 for unregistered users and therefore models SuperUser differently.

It uses the reserved identity:

```text
"SuperUser"
```

plus authenticated state.

This is not wire-equivalent for tools that assume Murmur user-ID semantics.

For Web-managed deployments, consider removing reliance on traditional SuperUser behavior entirely and use platform roles.

---

# 30. Recommended Remediation Order

## Phase 1 — Production blockers

```text
1. Redesign VoiceTarget
2. Fix stale session recipient problem
3. Add Whisper authorization
4. Rewrite ACL evaluator using Murmur semantics
5. Rewrite Group evaluator using Murmur semantics
6. Fix linked-channel recursive resolution
7. Add per-linked-channel Speak checks
8. Fix BanList authorization
9. Fix TextMessage subtree authorization
```

## Phase 2 — Compatibility hardening

```text
10. Temporary channel lifecycle
11. ChannelState field presence
12. Channel reparent
13. LinkChannel permission semantics
14. QueryUsers semantics
```

## Phase 3 — Optional native administration

```text
15. ACL.groups/acls wire support
16. UserList
17. UserStats
```

## Phase 4 — Modern protocol extensions

```text
18. MumbleUDP protobuf audio
19. correct VersionV1/VersionV2 advertisement
20. Channel Listener
21. PluginDataTransmission
22. ContextAction callbacks
```

---

# 31. Mandatory Regression Tests

Add tests for at least:

```text
VoiceTarget:
- direct user target
- target disconnect
- session reuse
- group target
- channel target
- children
- links
- Whisper ACL deny
- recipient deaf

ACL:
- Traverse parent deny
- Write child vs root
- root-only Kick/Ban/Register
- ApplyHere
- ApplySubs
- InheritACL=false

Groups:
- parent add / child remove
- parent remove / child add
- inherit=false
- inheritable=false
- auth
- none
- strong
- #token case-insensitive
- $certificate
- sub args
- invert
- eval-here

Linked voice:
A ↔ B ↔ C
Speak A allow
Speak B deny
Speak C allow

BanList:
- unauthorized query
- authorized query
- clear all bans

TextMessage:
- subtree with denied descendant

Temporary channels:
- create
- enter
- last user leaves
- restart

Protocol:
- Mumble Windows 1.5.915
- Mumble Linux 1.5.915
- Mumla/Android
- UDP
- TCP tunnel fallback
- reconnect
```

---

# 32. Recommended Production Position

Current upstream:

```text
NO-GO for sensitive production use without patching.
```

Recommended strategy:

```text
fork upstream
↓
fix P0/P1
↓
add Murmur behavioral regression tests
↓
run client interoperability testing
↓
deploy as Gin-managed Mumble backend
```

The project's architecture is sufficiently clean that fixing these gaps is still likely simpler and more maintainable for a Go-based platform than introducing ZeroC Ice + C++ + cgo around official Murmur.

The primary engineering objective should be:

> Preserve Mumble client compatibility while treating the Web platform, not the native Mumble administrative UI, as the authoritative control plane.