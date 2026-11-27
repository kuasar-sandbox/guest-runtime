[English](network-restore.md) | [简体中文](network-restore_zh.md)

# Platform network state on snapshot restore

A memory restore resumes local application state on a destination whose network
identity or remote peers may have changed. The Runtime refreshes the single
virtio-net interface supplied by sandboxer, including stale neighbors and eligible
old TCP connections. Applications remain responsible for reconnecting and renewing
external state. This page explains what happens automatically, what remains
application-owned and how to diagnose the boundary.

## Recommended restore workflow

1. Prepare a cold instance with the selected Runtime/Kernel and configure its
   platform network through sandboxer or the orchestrator/connector deployment.
   Register any application refresh handler before capture, using
   [restore notifications](using-guest-runtime.md#application-restore).
2. Capture the instance and restore it with the destination's Host network
   binding. No additional network-reset switch or Guest networking binary is
   needed. Do not add blanket route flushes or interface recreation for the
   platform-owned state described below.
3. After restore, verify a real request and the application's reconnect/lease
   logic. Platform readiness does not wait for remote peers or application
   reconnection. Listening sockets remain available; active connections selected
   for cleanup can fail so the application can establish new ones.
4. If a request fails, inspect Guest network logs and the current address/route,
   then separate an application retry failure from unsupported kernel cleanup or
   user-managed network state. A successful Restore alone does not prove all old
   TCP sockets were released.

The implementation uses the existing Host/Guest wire contract. It does not add
Host configuration switches, traverse user network namespaces or introduce a
kernel patch. The exact ownership and bounded behavior follow.

## Behavior

Outer init binds the platform NIC before running user initialization, including
launches with no IP configuration. The binding is private snapshot memory: the
original network namespace, interface index and sysfs object identity, plus the
exact default route that init successfully installed. A rename of that same
object works; a replacement device using its old name/index does not acquire
ownership. If the platform device has moved, disappeared or become a member of
a user master device, the network operation stops without device writes and
Restore continues under its existing best-effort network policy.

| Operation | Configuration | Dynamic neighbors | Old TCP |
| --- | --- | --- | --- |
| Cold launch | Apply supplied values | No reset | No reset |
| True Restore with a new IP | Replace platform IP/default | Clear platform ARP/NDP | Attempt release of the pre-change batch |
| True Restore with the same IP | Apply supplied values | Clear platform ARP/NDP | Attempt release of the pre-change batch |
| True Restore with absent/empty IP | Preserve IP/default; apply an explicitly supplied hostname | Clear platform ARP/NDP | Attempt release of the pre-change batch |
| Attach / same-VM reconnect | Preserve | Preserve | Preserve |

With a supplied IP, configuration replacement removes the platform NIC's other
global addresses in **both** families. An already matching address and prefix
are retained, avoiding Linux's dependent-route removal when the last IPv4
address is deleted. IPv6 link-local addresses are retained. Only
init's recorded default route is removed; no default-route table flush is used.
An empty gateway with a new IP removes the old owned default. A new default is
created exclusively and recorded only after a successful kernel ACK. An
unrelated colliding route produces a configuration error instead of being
silently replaced. Invalid CIDR, gateway family and MTU are rejected before
configuration writes.

Platform defaults use explicit nonzero metrics (IPv4: 1; IPv6: 1024). Linux
treats metric zero as a wildcard in deletion, so recording zero would not make
the delete key exact. These metrics preserve the ordinary single-default path;
user route priorities retain their normal Linux meaning.

A real IP/prefix change still has Linux's normal effects on connected routes
and routes dependent on the removed address. The platform does not reconstruct
user routes after that identity change.

Neighbor deletion covers IPv4 and IPv6 dynamic entries on the bound device.
Permanent, NOARP and proxy entries, bridge FDBs and entries on user devices are
outside that deletion. Enumeration must complete before deletes begin.

## TCP selection and release

Before changing addresses or routes, enumerate active full TCP sockets in the
outer init's original network namespace. The eligible states are ESTABLISHED,
SYN_SENT, SYN_RECV, FIN_WAIT1, FIN_WAIT2, CLOSE_WAIT, LAST_ACK and CLOSING.
LISTEN and TIME_WAIT are excluded. A socket must have a valid cookie, an old
platform local address, available route metadata and an ordinary unicast route
whose direct output interface is the bound platform NIC. An explicit socket
device binding to another NIC excludes it. An unbound socket does not implicitly
belong to the platform NIC.

The route query uses source, destination, ports, protocol, UID, mark and traffic
class. IPv4-mapped IPv6 sockets use IPv4 routing but retain their original IPv6
identity for destruction. Kernel full sockets with inode zero are eligible;
missing diagnostic metadata is not treated as zero. Multipath, nexthop objects
and encapsulated routes do not qualify as the existing simple platform path.

This includes application-created connections directly using the platform NIC.
Connections using a user-created virtual NIC or another network namespace remain
the user's responsibility. No recursive attribution through tunnels or arbitrary
packet rewriting is attempted, and merely adding a user interface does not
disable cleanup of ordinary platform connections.

After configuration and neighbor handling, send SOCK_DESTROY for the fixed
original socket IDs **including their cookies**. No second census, same-tuple
replacement lookup or retry-until-empty is performed. A vanished cookie is a
normal outcome. The TCP work shares one 50 ms monotonic deadline across dump,
route lookup and release; configuration/neighbor time between capture and release
also consumes that deadline. No peer response, application close, reconnect or
post-thaw cleanup is awaited. Netlink I/O is nonblocking and cancellation-aware.

## Failure and compatibility

TCP open, enumeration, classification, permission, unsupported capability and
release errors are logged separately. A malformed or incomplete census is not
used. One socket's release error does not force an unbounded retry; global
permission/capability failures and deadline expiry stop the remaining batch.
**Zero successful TCP releases is compatible with successful Restore.**
The normal memory-report transition, thaw, inner readiness and Restore ACK
continue. Their existing failures still prevent readiness. A pre-existing
Shutdown is still honored by the management lifecycle.

Configuration and neighbor errors retain Restore's best-effort policy and are
reported separately from TCP. The whole network operation has a one-second
deadline; the 50 ms deadline bounds TCP's extra work rather than granting that
budget to each connection. Scheduling delays can extend observed wall time, so
the configured deadline is not a hard real-time SLA.

The current x86_64 and arm64 kernel configuration enables `INET_DIAG`,
`INET_TCP_DIAG` and `INET_DIAG_DESTROY`. No new network namespaces, veth, BPF,
Netfilter or userspace networking binary is required. An older kernel without
destruction support continues Restore and records the unsupported TCP attempt.
Already captured snapshots keep their captured init and kernel: installing a new
runtime cannot retrofit this behavior into an old snapshot. Create a fresh
snapshot with the new runtime to receive the feature.

| Observed result | Interpretation and next check |
| --- | --- |
| Same-IP restore still loses an old connection | True restore also clears selected old platform TCP when the IP is unchanged. Check that the application retries. |
| Source `snapshot --resume` keeps a connection | Same-VM attach preserves network state; it is not a true restore notification. |
| Restore succeeds with a TCP permission, unsupported or timeout log | Cleanup is best effort. Check the captured kernel and the application retry path; do not infer full cleanup from ACK. |
| A user bridge, tunnel, namespace or static neighbor remains | These are outside platform cleanup. Maintain them through the owning application or network component. |
| A fresh Runtime was installed but an old snapshot behaves as before | The snapshot retains its captured init/kernel. Cold-start and capture new state with the selected pair. |

## Validation

Unit tests cover complete Netlink dump/ACK handling, sequence/port checks,
truncation and errors, finite deadlines, address-family replacement, exact
default ownership, neighbor exclusions, route and socket identity, mapped
sockets, vanished cookies and nonfatal release outcomes. Restore handler tests
inject unsupported, permission, I/O and timeout errors and verify thaw, readiness
and Restore ACK. Attach is tested independently to exclude network reset.

Real network tests must run in disposable process-owned network namespaces with
the required capabilities. They must exercise this Go implementation, not a
separate packet classifier. Real VM validation uses the production configuration
and the newly built init/kernel, with baseline/feature pairs measuring the Guest
network phase separately from Host restore readiness. Report observed sample
counts and quantiles; historical prototype timings are not production results.
