# 0009: Channel Listening

## Status: Implemented

Implements Mumble 1.4+ *channel listening*: a user hears a channel's voice
traffic without joining it. Until now `handleUserState` stripped the listening
fields on receipt (see the note this document replaces at the old strip site):
the wire codec, `PermissionListen` (0x800) and deny types 12/13 already
existed, but no state was tracked and listeners were never routed audio.

## Summary

A zero-value-usable `listenerManager` (`internal/mumble/listeners.go`) keeps a
dual index — session→channels and channel→sessions — plus per-(session,
channel) volume adjustments. `handleUserState` pulls the three listening fields
aside **before** the has-bit gates (they are repeated fields without presence
bits, so a listening-only message has `SetFields == 0` and would otherwise be
dropped) and hands them to `applyListening`, which enforces permissions and
limits, mutates the index, and broadcasts the applied delta. Voice routing
consults the channel→sessions index everywhere an occupant scan happens:
regular speech (target 0, including linked channels) and whisper/shout channel
targets (with the same group restriction as occupants).

## Murmur reference

| Behaviour | Murmur reference | Here |
| --- | --- | --- |
| Listening fields aimed at another session are dropped silently | `Messages.cpp` `msgUserState` | same, before any other listening work |
| Unknown channel IDs in `listening_channel_add` are skipped | same | same |
| Each added channel needs `Listen` there; denial is sent to the sender only and the rest of the message still applies | `Messages.cpp`, `PERM_DENIED(..., ChanACL::Listen)` | `PermissionDenied{Permission, permission=0x800, channel_id, session}` via `applyListening` |
| Channel/user listener caps deny with types 12/13 | `iMaxListenersPerChannel` / `iMaxListenerProxiesPerUser` | `max_listeners_per_channel` / `max_listeners_per_user` (toml `[server]`, env `MUMBLE_MAX_LISTENERS_*`, DB `server_configs`), 0 = unlimited, unlimited by default like murmur's -1 |
| Target 0 speech reaches listeners of the speaker's channel and of linked channels | `Server::processMsg` | `Router.Route` case 0 via the new `GetListenersInChan` hook, deduped against occupants by the existing `seen` set |
| Whisper/shout channel targets include listeners, group filter applies on the listened channel | `createWhisperTargetCacheFor` | `resolveVoiceTarget` channel loop |
| Deafened listeners receive nothing | `AudioReceiverBuffer::addReceiver` | existing `audioFilterRecipient` (Deaf/SelfDeaf) — reused unchanged |
| Disconnect removes all listened channels silently | `Server::messageDisconnected` | `UnregisterConn` → `RemoveAllFor`; the `UserRemove` broadcast already implies the list is gone |
| Channel deletion strips listeners and tells clients with a `listening_channel_remove` UserState **before** the `ChannelRemove` | `Server::removeChannel` | `handleChannelRemove`, after the occupant-move loop, before `chans.Remove` |
| Login sync carries every user's listening list; volumes are owner-only | `broadcastListenerVolumeAdjustments = false` default | `sendSync` attaches `ChannelsFor` to every roster snapshot and `ListeningVolumes` to the syncing user's own only |
| Volume adjustment for a channel not being listened to is warned and ignored | `setChannelListenerVolume` | `SetVolume` no-ops without a listening relationship |

## Known deviations

1. **Truthful deltas instead of echo.** Murmur broadcasts the *original*
   `UserState` back — including `listening_channel_add` entries that were
   denied — and only stores the permitted subset, so other clients render
   listening state the server never established. Here the broadcast contains
   only what was actually applied, consistent with this server's standing rule
   (formerly codified at the strip site): never announce a state that was not
   established.
2. **Volume is state, not payload.** Murmur 1.5 inserts the volume factor into
   protobuf-UDP voice packets (`MumbleUDP.proto Audio.volume_adjustment`) and
   the client applies the gain. This server speaks the legacy varint voice
   format only, which cannot carry the factor, so adjustments are stored,
   synced to the owner, and applied client-side — the same behaviour murmur
   has for legacy-UDP clients.
3. **No persistence.** Murmur stores listeners for registered users and
   restores them at login. Listening here is session state: a reconnect starts
   with an empty list (the join broadcast needs no listening fields, which is
   why `userToState` stays pure and `sendSync` attaches the lists at its call
   sites).

## Locking

`listenerManager` owns one `RWMutex` and never calls back into `users`,
`chans` or `conns` while holding it — ID sets are snapshotted under the lock
and resolved outside, the same discipline as `voicetarget.go`. All accessors
return freshly built sorted slices, so callers can mutate or broadcast them
freely.

## Regression coverage

`internal/mumble/handlers_listening_test.go`, all against a real server with a
real sqlite ACL evaluator:

- `TestUserStateListeningAddBroadcastsAppliedDelta` — a listening-only
  UserState (`SetFields == 0`) must survive the has-bit gates; guards the
  ordering that makes the feature work at all
- `TestUserStateListeningAddUnknownChannelSkipped`
- `TestUserStateListeningDenyListenPermissionPartialSuccess` — denial carries
  `permission=0x800` and precedes the broadcast of the surviving channel
- `TestUserStateListeningTargetingOtherSessionIgnored`
- `TestUserStateListeningRemoveBroadcasts`
- `TestUserStateListeningVolumeStoredAndOwnerOnly`
- `TestUserStateListeningVolumeWithoutListenIgnored`
- `TestVoiceRoutesToListenersTargetZero` — also covers deafened listeners and
  same-channel non-listeners
- `TestVoiceRoutesToLinkedChannelListeners`
- `TestWhisperChannelTargetIncludesListeners` — including `#token` group
  filtering of listeners
- `TestUnregisterConnDropsListeners`
- `TestChannelRemoveDropsListenersWithUserStateBeforeChannelRemove` — asserts
  message order
- `TestSendSyncIncludesListeningLists` — also asserts volumes do not leak to
  other users' snapshots
- `TestListenerLimitDenials` — both deny types 12 and 13
