#!/usr/bin/env python3
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Optional


WAIT_SECONDS = 60
PUBLIC_HOST = "host.docker.internal"


@dataclass
class Node:
    label: str
    base: str
    container: str
    server_name: str
    mxid: str
    password: str
    token: str = ""
    agent_token: str = ""
    name: str = ""
    avatar: str = ""
    request_seq: int = 0


class ApiError(Exception):
    def __init__(self, status: int, body: Any, message: str):
        super().__init__(message)
        self.status = status
        self.body = body


def run(args: list[str]) -> str:
    return subprocess.check_output(args, text=True).strip()


def run_checked(args: list[str]) -> None:
    subprocess.check_call(args)


def read_bootstrap(container: str) -> dict[str, Any]:
    last_error = ""
    for _ in range(60):
        try:
            raw = run(["docker", "exec", container, "cat", "/var/dirextalk-message-server/p2p/bootstrap.json"])
            return dict(json.loads(raw))
        except (subprocess.CalledProcessError, json.JSONDecodeError) as exc:
            last_error = str(exc)
            time.sleep(1)
    raise RuntimeError(f"{container} bootstrap.json was not readable: {last_error}")


def request_json(method: str, url: str, body: Any = None, token: str = "") -> Any:
    data = None if body is None else json.dumps(body).encode("utf-8")
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as res:
            raw = res.read().decode("utf-8")
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8")
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = raw
        raise ApiError(exc.code, parsed, f"{method} {url} failed with {exc.code}") from exc


def p2p(node: Node, kind: str, action: str, params: Optional[dict[str, Any]] = None, *, token: Optional[str] = None) -> Any:
    bearer = node.token if token is None else token
    try:
        return request_json(
            "POST",
            f"{node.base}/_p2p/{kind}",
            {"action": action, "params": params or {}},
            bearer,
        )
    except ApiError as exc:
        if exc.status != 401 or token is not None:
            raise
        login(node)
        return request_json(
            "POST",
            f"{node.base}/_p2p/{kind}",
            {"action": action, "params": params or {}},
            node.token,
        )


def p2p_status(node: Node, kind: str, action: str, params: Optional[dict[str, Any]] = None, *, token: Optional[str] = None) -> tuple[int, Any]:
    try:
        return 200, p2p(node, kind, action, params, token=token)
    except ApiError as exc:
        return exc.status, exc.body


MCP_TOOL_BY_ACTION = {
    "mcp.rooms.search": "dirextalk_rooms_search",
    "mcp.messages.send": "dirextalk_messages_send",
    "mcp.messages.list": "dirextalk_messages_list",
    "mcp.channel_posts.list": "dirextalk_channel_posts_list",
    "mcp.channel_comments.create": "dirextalk_channel_comments_create",
    "mcp.channel_comments.list": "dirextalk_channel_comments_list",
}


def mcp(node: Node, action: str, params: Optional[dict[str, Any]] = None) -> Any:
    tool_name = MCP_TOOL_BY_ACTION.get(action)
    if not tool_name:
        raise RuntimeError(f"unknown MCP regression action {action}")
    node.request_seq += 1
    response = request_json(
        "POST",
        f"{node.base}/mcp",
        {
            "jsonrpc": "2.0",
            "id": f"{node.label.lower()}-mcp-{node.request_seq}",
            "method": "tools/call",
            "params": {
                "name": tool_name,
                "arguments": params or {},
            },
        },
        node.agent_token,
    )
    if response.get("error"):
        raise RuntimeError(f"MCP {action} failed: {response['error']}")
    result = response.get("result") or {}
    if result.get("isError"):
        raise RuntimeError(f"MCP {action} returned error result: {result}")
    return result.get("structuredContent") or {}


def matrix_send_text(node: Node, room_id: str, text: str, txn_id: Optional[str] = None) -> str:
    room_path = urllib.parse.quote(room_id, safe="")
    txn_id = txn_id or f"three_node_{int(time.time() * 1000)}"
    body = {"msgtype": "m.text", "body": text}
    res = request_json(
        "PUT",
        f"{node.base}/_matrix/client/v3/rooms/{room_path}/send/m.room.message/{txn_id}",
        body,
        node.token,
    )
    return res.get("event_id", "")


def wait_matrix_send_text(node: Node, room_id: str, text: str, label: str, seconds: int = WAIT_SECONDS) -> str:
    txn_id = f"three_node_{int(time.time() * 1000)}"

    return wait_until(
        label,
        lambda: matrix_send_text(node, room_id, text, txn_id=txn_id),
        seconds=seconds,
    )


def matrix_messages(node: Node, room_id: str) -> list[Any]:
    room_path = urllib.parse.quote(room_id, safe="")
    res = request_json(
        "GET",
        f"{node.base}/_matrix/client/v3/rooms/{room_path}/messages?dir=b&limit=30",
        token=node.token,
    )
    return list(res.get("chunk") or [])


def wait_until(label: str, fn, seconds: int = WAIT_SECONDS):
    deadline = time.monotonic() + seconds
    last_error: Optional[Exception] = None
    while time.monotonic() < deadline:
        try:
            value = fn()
            if value:
                return value
        except Exception as exc:  # keep polling federation/projection lag
            last_error = exc
        time.sleep(0.75)
    if last_error:
        raise AssertionError(f"{label}: {last_error}") from last_error
    raise AssertionError(label)


def expect(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def find_by(items: list[Any], **expected) -> Optional[Any]:
    for item in items:
        if all(item.get(key) == value for key, value in expected.items()):
            return item
    return None


def conversations(node: Node) -> list[dict[str, Any]]:
    res = p2p(node, "query", "conversations.list")
    return list(res.get("conversations") or [])


def conversation_for(node: Node, room_id: str) -> Optional[dict[str, Any]]:
    return find_by(conversations(node), matrix_room_id=room_id)


def assert_caps(
    conversation: dict[str, Any],
    *,
    send: bool,
    send_media: bool,
    call: bool,
    label: str,
    extra: Optional[dict[str, bool]] = None,
) -> None:
    caps = conversation.get("capabilities") or {}
    expected_caps = {
        "open": True,
        "send": send,
        "send_media": send_media,
        "call": call,
    }
    expected_caps.update(extra or {})
    for key, expected in expected_caps.items():
        expect(caps.get(key) is expected, f"{label} capability {key} expected {expected}, got {caps.get(key)!r}")


def login(node: Node) -> None:
    auth = p2p(
        node,
        "query",
        "portal.auth",
        {"password": node.password, "device_id": f"THREE-{node.label}"},
        token="",
    )
    node.token = auth.get("access_token") or ""
    expect(bool(node.token), f"{node.label} portal.auth did not return access_token")


def update_profile(node: Node, suffix: int) -> None:
    node.name = f"{node.label} Three {suffix}"
    node.avatar = f"mxc://{node.server_name}/three-{node.label.lower()}-{suffix}"
    profile = p2p(
        node,
        "command",
        "profile.update",
        {"display_name": node.name, "avatar_url": node.avatar},
    )
    expect(profile.get("display_name") == node.name, f"{node.label} profile.update did not persist display name")
    login(node)


def accept_direct_contact(accepter: Node, params: dict[str, Any], label: str) -> Any:
    accepted = p2p(accepter, "command", "contacts.requests.accept", params)
    if accepted.get("status") == "accepted":
        return accepted
    expect(accepted.get("status") == "joining", f"{label} returned unexpected status {accepted.get('status')!r}")

    retry_params = dict(params)
    if operation_id := accepted.get("operation_id"):
        retry_params["operation_id"] = operation_id

    def retry_until_accepted() -> Optional[Any]:
        retry = p2p(accepter, "command", "contacts.requests.accept", retry_params)
        if retry.get("status") == "accepted":
            return retry
        expect(retry.get("status") == "joining", f"{label} retry returned unexpected status {retry.get('status')!r}")
        return None

    return wait_until(f"{label} did not reconcile", retry_until_accepted)


def ensure_direct(a: Node, b: Node) -> str:
    contact = p2p(a, "command", "contacts.request", {"mxid": b.mxid, "display_name": b.name})
    room_id = contact.get("room_id") or ""
    expect(bool(room_id), "contacts.request did not return room_id")
    if contact.get("status") == "pending_outbound":
        wait_until(
            "B did not receive inbound contact request",
            lambda: find_by(
                list((p2p(b, "command", "sync.bootstrap").get("contacts") or [])),
                room_id=room_id,
                status="pending_inbound",
            ),
        )
        accepted = accept_direct_contact(
            b,
            {"room_id": room_id, "peer_mxid": a.mxid, "display_name": a.name, "domain": a.server_name},
            "contacts.requests.accept",
        )
    else:
        expect(contact.get("status") == "accepted", f"unexpected direct contact status {contact.get('status')!r}")

    wait_until(
        "A did not project accepted direct conversation",
        lambda: (conversation_for(a, room_id) or {}).get("relationship_status") == "accepted",
    )
    try:
        wait_until(
            "B did not project accepted direct conversation before repair",
            lambda: (conversation_for(b, room_id) or {}).get("relationship_status") == "accepted",
            seconds=WAIT_SECONDS,
        )
    except AssertionError:
        restored = p2p(b, "command", "contacts.request", {"mxid": a.mxid, "display_name": a.name})
        expect(restored.get("status") == "accepted", "peer-side contacts.request did not repair accepted direct state")
        expect(restored.get("room_id") == room_id, "peer-side contacts.request did not reuse direct room")
    wait_until(
        "B did not project accepted direct conversation",
        lambda: (conversation_for(b, room_id) or {}).get("relationship_status") == "accepted",
    )
    assert_caps(conversation_for(a, room_id) or {}, send=True, send_media=True, call=True, label="A direct")
    assert_caps(conversation_for(b, room_id) or {}, send=True, send_media=True, call=True, label="B direct")
    return room_id


def verify_delete_readd(deleter: Node, peer: Node, room_id: str) -> None:
    deleted = p2p(deleter, "command", "contacts.delete", {"room_id": room_id})
    expect(deleted.get("status") == "ok", "contacts.delete did not return ok")
    restored = p2p(deleter, "command", "contacts.request", {"mxid": peer.mxid, "display_name": peer.name})
    expect(restored.get("status") == "accepted", "deleted contact re-request did not restore accepted state")
    expect(restored.get("room_id") == room_id, "deleted contact re-request did not reuse old direct room")
    wait_until(
        "restored direct conversation did not reopen",
        lambda: (conversation_for(deleter, room_id) or {}).get("relationship_status") == "accepted",
    )


def verify_mutual_delete_readd(requester: Node, accepter: Node, room_id: str, suffix: int) -> None:
    deleted_requester = p2p(requester, "command", "contacts.delete", {"room_id": room_id})
    expect(deleted_requester.get("status") == "ok", "requester contacts.delete did not return ok")
    deleted_accepter = p2p(accepter, "command", "contacts.delete", {"room_id": room_id})
    expect(deleted_accepter.get("status") == "ok", "accepter contacts.delete did not return ok")

    request = p2p(requester, "command", "contacts.request", {"mxid": accepter.mxid, "display_name": accepter.name})
    request_room = request.get("room_id") or ""
    expect(request_room, "mutual delete contacts.request did not return room_id")
    expect(request.get("status") == "pending_outbound", f"mutual delete must restart approval flow, got {request.get('status')!r}")
    expect(request_room != room_id, "mutual delete must create a replacement direct room")
    wait_until(
        "accepter did not receive mutual-delete inbound contact request",
        lambda: find_by(
            list((p2p(accepter, "command", "sync.bootstrap").get("contacts") or [])),
            peer_mxid=requester.mxid,
            room_id=request_room,
            status="pending_inbound",
        ),
    )
    accepted = accept_direct_contact(
        accepter,
        {"room_id": request_room, "peer_mxid": requester.mxid, "display_name": requester.name, "domain": requester.server_name},
        "mutual-delete contacts.requests.accept",
    )

    requester_contact = wait_until(
        "requester did not converge to accepted contact after mutual-delete accept",
        lambda: find_by(
            list((p2p(requester, "command", "sync.bootstrap").get("contacts") or [])),
            peer_mxid=accepter.mxid,
            room_id=request_room,
            status="accepted",
        ),
    )
    wait_until(
        "accepter did not converge to accepted contact after mutual-delete accept",
        lambda: find_by(
            list((p2p(accepter, "command", "sync.bootstrap").get("contacts") or [])),
            peer_mxid=requester.mxid,
            room_id=request_room,
            status="accepted",
        ),
    )
    requester_contacts = list((p2p(requester, "command", "sync.bootstrap").get("contacts") or []))
    expect(
        not find_by(requester_contacts, peer_mxid=accepter.mxid, status="pending_inbound"),
        "requester should not receive a duplicate inbound request after its request is accepted",
    )
    text = f"mutual delete readd direct message {suffix}"
    event_id = wait_matrix_send_text(
        accepter,
        requester_contact.get("room_id") or request_room,
        text,
        "Matrix direct send after mutual-delete readd did not return event_id",
    )
    assert_history(requester, requester_contact.get("room_id") or request_room, text)


def rebuild_node_with_empty_state(node: Node, suffix: int) -> None:
    suffix_name = node.label.lower()
    run_checked(["docker", "compose", "-f", "docker-compose.p2p-dual.yml", "rm", "-f", "-s", "-v", f"dendrite-{suffix_name}", f"dendrite-{suffix_name}-init", f"postgres-{suffix_name}"])
    # The Matrix signing key is the server's durable identity. Reusing a
    # server_name with a regenerated key makes its own historical events
    # cryptographically unverifiable, so a state-loss recovery must preserve
    # the config volume while replacing PostgreSQL and application data.
    for volume in [
        f"dirextalk-p2p-dual_p2p_dual_postgres_{suffix_name}",
        f"dirextalk-p2p-dual_p2p_dual_message_server_{suffix_name}_data",
    ]:
        run_checked(["docker", "volume", "rm", volume])
    run_checked(["docker", "compose", "-f", "docker-compose.p2p-dual.yml", "up", "-d", "--force-recreate", f"dendrite-{suffix_name}"])
    wait_until(f"{node.label} did not become healthy after empty-state rebuild", lambda: request_json("GET", f"{node.base}/_p2p/health").get("status") == "ok", seconds=120)
    bootstrap = read_bootstrap(node.container)
    node.password = bootstrap.get("password") or ""
    node.agent_token = bootstrap.get("agent_token") or ""
    expect(bool(node.password), f"{node.label} rebuilt bootstrap did not include password")
    expect(bool(node.agent_token), f"{node.label} rebuilt bootstrap did not include agent_token")
    login(node)
    update_profile(node, suffix)


def verify_rebuilt_peer_readd(retained_peer: Node, rebuilt_peer: Node, suffix: int) -> None:
    old_room = ensure_direct(retained_peer, rebuilt_peer)
    text = f"pre-rebuild direct message {suffix}"
    event_id = wait_matrix_send_text(
        retained_peer,
        old_room,
        text,
        "Matrix direct send before rebuild did not return event_id",
    )
    assert_history(rebuilt_peer, old_room, text)

    rebuild_node_with_empty_state(rebuilt_peer, suffix + 1)
    request = p2p(
        rebuilt_peer,
        "command",
        "contacts.request",
        {
            "mxid": retained_peer.mxid,
            "display_name": retained_peer.name,
            "domain": retained_peer.server_name,
            "remote_node_base_url": f"{retained_peer.base.replace('127.0.0.1', PUBLIC_HOST)}/_p2p",
        },
    )
    request_room = request.get("room_id") or ""
    expect(request_room, "rebuilt peer contacts.request did not return room_id")

    rebuilt_contact = wait_until(
        "rebuilt peer did not restore accepted contact with retained peer",
        lambda: find_by(list((p2p(rebuilt_peer, "command", "sync.bootstrap").get("contacts") or [])), peer_mxid=retained_peer.mxid, status="accepted"),
        seconds=90,
    )
    rebuilt_room = rebuilt_contact.get("room_id")
    retained_contact = wait_until(
        "retained peer did not converge to rebuilt peer direct room after peer rebuild",
        lambda: find_by(
            list((p2p(retained_peer, "command", "sync.bootstrap").get("contacts") or [])),
            peer_mxid=rebuilt_peer.mxid,
            status="accepted",
            room_id=rebuilt_room,
        ),
        seconds=90,
    )
    retained_room = retained_contact.get("room_id")
    expect(rebuilt_room == retained_room, f"peers disagree on restored direct room: rebuilt={rebuilt_room!r} retained={retained_room!r}")
    if rebuilt_room == old_room:
        assert_history(rebuilt_peer, old_room, text)
    else:
        expect(rebuilt_room == request_room, f"rebuilt peer should use replacement direct room {request_room!r}, got {rebuilt_room!r}")


def create_group(owner: Node, name: str) -> str:
    group = p2p(owner, "command", "groups.create", {"name": name, "topic": "three-node regression"})
    room_id = group.get("room_id") or ""
    expect(bool(room_id), f"groups.create {name} did not return room_id")
    return room_id


def invite_and_join(owner: Node, invitee: Node, room_id: str) -> None:
    p2p(owner, "command", "groups.invite", {"room_id": room_id, "user_id": invitee.mxid, "display_name": invitee.name})
    wait_until(
        f"{invitee.label} did not receive group invite",
        lambda: find_by(list((p2p(invitee, "command", "sync.bootstrap").get("pending", {}).get("group_invites") or [])), id=room_id),
    )
    joined = p2p(
        invitee,
        "command",
        "groups.join",
        {"room_id": room_id, "server_names": [owner.server_name], "display_name": invitee.name, "avatar_url": invitee.avatar},
    )
    expect(joined.get("status") == "ok", f"{invitee.label} groups.join did not return ok")
    wait_until(
        f"{invitee.label} groups.list missing joined group",
        lambda: find_by(list((p2p(invitee, "command", "groups.list").get("groups") or [])), room_id=room_id),
    )


def create_channel(owner: Node, name: str, visibility: str, join_policy: str, channel_type: str = "chat") -> dict[str, Any]:
    channel = p2p(
        owner,
        "command",
        "channels.create",
        {
            "name": name,
            "visibility": visibility,
            "join_policy": join_policy,
            "channel_type": channel_type,
            "comments_enabled": channel_type == "post",
        },
    )
    expect(bool(channel.get("room_id") and channel.get("channel_id")), f"channels.create {name} did not return identifiers")
    return channel


def invite_channel_and_join(owner: Node, invitee: Node, channel: dict[str, Any]) -> None:
    room_id = channel.get("room_id") or ""
    channel_id = channel.get("channel_id") or ""
    p2p(
        owner,
        "command",
        "channels.invite",
        {"room_id": room_id, "channel_id": channel_id, "user_id": invitee.mxid, "display_name": invitee.name},
    )
    wait_until(
        f"{invitee.label} did not receive channel invite",
        lambda: find_by(list((p2p(invitee, "command", "sync.bootstrap").get("pending", {}).get("channel_notices") or [])), id=room_id),
    )
    joined = p2p(
        invitee,
        "command",
        "channels.join",
        {"room_id": room_id, "channel_id": channel_id, "server_names": [owner.server_name], "display_name": invitee.name, "avatar_url": invitee.avatar},
    )
    expect(joined.get("status") == "ok", f"{invitee.label} channels.join did not return ok")
    wait_until(
        f"{invitee.label} channels.list missing joined channel",
        lambda: find_by(list((p2p(invitee, "command", "channels.list").get("channels") or [])), room_id=room_id),
    )
    wait_until(
        f"{invitee.label} conversations missing joined channel",
        lambda: conversation_for(invitee, room_id),
    )


def public_channel_join_request(owner: Node, joiner: Node, channel: dict[str, Any]) -> dict[str, Any]:
    return p2p(
        joiner,
        "command",
        "channels.public.join_request",
        {
            "room_id": channel.get("room_id"),
            "channel_id": channel.get("channel_id"),
            "remote_node_base_url": f"{owner.base.replace('127.0.0.1', PUBLIC_HOST)}/_p2p",
            "requester_node_base_url": f"{joiner.base.replace('127.0.0.1', PUBLIC_HOST)}/_p2p",
            "server_names": [owner.server_name],
            "display_name": joiner.name,
            "avatar_url": joiner.avatar,
        },
    )


def wait_public_channel_join(owner: Node, joiner: Node, channel: dict[str, Any], label: str) -> dict[str, Any]:
    last: dict[str, Any] = public_channel_join_request(owner, joiner, channel)
    initial = last
    if last.get("status") != "joined":
        def joined():
            nonlocal last
            current = find_by(list((p2p(joiner, "command", "channels.list").get("channels") or [])), room_id=channel.get("room_id"))
            if current:
                last = current
                if current.get("member_status") in ("join", "joined", ""):
                    return current
            return None

        last = wait_until(
            f"{joiner.label} {label} public channel join did not reach joined, initial={initial!r} last={last!r}",
            joined,
            seconds=120,
        )
    wait_until(
        f"{joiner.label} {label} conversations missing joined public channel",
        lambda: conversation_for(joiner, channel.get("room_id")),
        seconds=120,
    )
    return last


def verify_rebuilt_room_reentry(owner: Node, rebuilt_peer: Node, suffix: int) -> None:
    group_room = create_group(owner, f"Retained Group {suffix}")
    invite_and_join(owner, rebuilt_peer, group_room)
    group_text = f"pre-rebuild retained group message {suffix}"
    wait_matrix_send_text(owner, group_room, group_text, "Matrix retained group send did not return event_id")
    assert_history(rebuilt_peer, group_room, group_text)

    private_channel = create_channel(owner, f"Retained Private {suffix}", "private", "invite")
    invite_channel_and_join(owner, rebuilt_peer, private_channel)

    public_channel = create_channel(owner, f"Retained Public {suffix}", "public", "open", "post")
    post = p2p(
        owner,
        "command",
        "channels.posts.create",
        {
            "channel_id": public_channel.get("channel_id"),
            "room_id": public_channel.get("room_id"),
            "body": f"pre-rebuild public post {suffix}",
            "message_type": "text",
        },
    )
    expect(bool(post.get("post_id")), "public channel pre-rebuild post did not return post_id")
    wait_public_channel_join(owner, rebuilt_peer, public_channel, "initial")
    public_chat_channel = create_channel(owner, f"Retained Public Chat {suffix}", "public", "open", "chat")
    wait_public_channel_join(owner, rebuilt_peer, public_chat_channel, "initial chat")

    rebuild_node_with_empty_state(rebuilt_peer, suffix + 2)

    p2p(
        owner,
        "command",
        "groups.invite",
        {
            "room_id": group_room,
            "user_id": rebuilt_peer.mxid,
            "display_name": rebuilt_peer.name,
            "remote_node_base_url": f"{rebuilt_peer.base.replace('127.0.0.1', PUBLIC_HOST)}/_p2p",
            # An empty-state rebuild is the only flow allowed to replace an
            # otherwise joined Matrix membership. Keep this fence stable for
            # retries of the same recovery attempt.
            "rebuild_generation": f"rebuild_group_{suffix}",
        },
    )
    wait_until(
        f"{rebuilt_peer.label} did not receive retained group re-invite",
        lambda: find_by(list((p2p(rebuilt_peer, "command", "sync.bootstrap").get("pending", {}).get("group_invites") or [])), id=group_room),
        seconds=90,
    )
    group_join = p2p(rebuilt_peer, "command", "groups.join", {"room_id": group_room, "server_names": [owner.server_name], "display_name": rebuilt_peer.name, "avatar_url": rebuilt_peer.avatar})
    expect(group_join.get("status") == "ok", f"{rebuilt_peer.label} retained group join failed: {group_join!r}")
    wait_until(
        f"{rebuilt_peer.label} groups.list missing retained group after rejoin",
        lambda: find_by(list((p2p(rebuilt_peer, "command", "groups.list").get("groups") or [])), room_id=group_room),
        seconds=90,
    )
    group_rejoin_text = f"post-rebuild retained group message {suffix}"
    wait_matrix_send_text(
        rebuilt_peer,
        group_room,
        group_rejoin_text,
        "Matrix retained group send after rejoin did not return event_id",
    )
    assert_history(owner, group_room, group_rejoin_text)

    p2p(
        owner,
        "command",
        "channels.invite",
        {
            "room_id": private_channel.get("room_id"),
            "channel_id": private_channel.get("channel_id"),
            "user_id": rebuilt_peer.mxid,
            "display_name": rebuilt_peer.name,
            "remote_node_base_url": f"{rebuilt_peer.base.replace('127.0.0.1', PUBLIC_HOST)}/_p2p",
            "rebuild_generation": f"rebuild_channel_{suffix}",
        },
    )
    wait_until(
        f"{rebuilt_peer.label} did not receive retained private channel re-invite",
        lambda: find_by(list((p2p(rebuilt_peer, "command", "sync.bootstrap").get("pending", {}).get("channel_notices") or [])), id=private_channel.get("room_id")),
        seconds=90,
    )
    private_join = p2p(
        rebuilt_peer,
        "command",
        "channels.join",
        {
            "room_id": private_channel.get("room_id"),
            "channel_id": private_channel.get("channel_id"),
            "server_names": [owner.server_name],
            "display_name": rebuilt_peer.name,
            "avatar_url": rebuilt_peer.avatar,
        },
    )
    expect(private_join.get("status") == "ok", f"{rebuilt_peer.label} retained private channel join failed: {private_join!r}")
    wait_until(
        f"{rebuilt_peer.label} channels.list missing retained private channel after rejoin",
        lambda: find_by(list((p2p(rebuilt_peer, "command", "channels.list").get("channels") or [])), room_id=private_channel.get("room_id")),
        seconds=90,
    )
    private_channel_text = f"post-rebuild private channel message {suffix}"
    wait_matrix_send_text(
        rebuilt_peer,
        private_channel.get("room_id") or "",
        private_channel_text,
        "Matrix private channel send after rejoin did not return event_id",
    )
    assert_history(owner, private_channel.get("room_id") or "", private_channel_text)

    wait_public_channel_join(owner, rebuilt_peer, public_channel, "retained")
    assert_matrix_channel_content(rebuilt_peer, public_channel.get("room_id") or "", [f"pre-rebuild public post {suffix}"])
    wait_public_channel_join(owner, rebuilt_peer, public_chat_channel, "retained chat")
    public_chat_text = f"post-rebuild public chat channel message {suffix}"
    wait_matrix_send_text(
        rebuilt_peer,
        public_chat_channel.get("room_id") or "",
        public_chat_text,
        "Matrix public chat channel send after rejoin did not return event_id",
    )
    assert_history(owner, public_chat_channel.get("room_id") or "", public_chat_text)


def assert_members(owner: Node, room_id: str, expected: list[Node]) -> None:
    def ready():
        members = list((p2p(owner, "command", "groups.members", {"room_id": room_id}).get("members") or []))
        for node in expected:
            match = find_by(members, user_id=node.mxid, membership="join")
            if not match or match.get("display_name") != node.name:
                return None
        return members

    members = wait_until("group members did not project joined display names", ready)
    owner_names = [m.get("display_name") for m in members if m.get("display_name") == "owner"]
    expect(not owner_names, "group members collapsed to fallback display name 'owner'")


def assert_message_projection(nodes: list[Node], room_id: str, text: str) -> None:
    for node in nodes:
        conv = wait_until(
            f"{node.label} conversation preview did not receive group message",
            lambda n=node: conversation_for(n, room_id) if (conversation_for(n, room_id) or {}).get("last_message") == text else None,
        )
        assert_caps(conv, send=True, send_media=True, call=True, label=f"{node.label} group")


def assert_history(node: Node, room_id: str, text: str) -> None:
    def has_message():
        for event in matrix_messages(node, room_id):
            if (event.get("content") or {}).get("body") == text:
                return True
        return False

    wait_until(f"{node.label} Matrix /messages did not return historical group message", has_message)


def assert_matrix_channel_content(node: Node, room_id: str, expected_bodies: list[str]) -> None:
    def has_channel_content():
        bodies = []
        for event in matrix_messages(node, room_id):
            content = event.get("content") or {}
            if content.get("p2p_kind") in {"channel_post", "channel_comment"}:
                bodies.append(content.get("body"))
        return all(body in bodies for body in expected_bodies)

    wait_until(f"{node.label} Matrix /messages did not return historical channel content", has_channel_content)


def contact_visible(node: Node, peer_mxid: str, room_id: str) -> Optional[dict[str, Any]]:
    contacts = list((p2p(node, "command", "sync.bootstrap").get("contacts") or []))
    return find_by(contacts, peer_mxid=peer_mxid, room_id=room_id)


def group_visible(node: Node, room_id: str) -> Optional[dict[str, Any]]:
    return find_by(list((p2p(node, "command", "groups.list").get("groups") or [])), room_id=room_id)


def channel_visible(node: Node, room_id: str) -> Optional[dict[str, Any]]:
    return find_by(list((p2p(node, "command", "channels.list").get("channels") or [])), room_id=room_id)


def joined_member(node: Node, action: str, params: dict[str, Any], user_id: str) -> Optional[dict[str, Any]]:
    members = list((p2p(node, "command", action, params).get("members") or []))
    member = find_by(members, user_id=user_id)
    if member and member.get("membership") in ("join", "joined"):
        return member
    return None


def room_removed_from_product_views(node: Node, room_id: str, kind: str) -> bool:
    if kind == "group" and group_visible(node, room_id):
        return False
    if kind == "channel" and channel_visible(node, room_id):
        return False
    conversation = conversation_for(node, room_id)
    if not conversation:
        return True
    if conversation.get("lifecycle") == "dissolved":
        caps = conversation.get("capabilities") or {}
        return not caps.get("open") and not caps.get("send")
    return False


def verify_account_delete_deprovision_propagates(deleter: Node, peer: Node, observer: Node, suffix: int) -> None:
    direct_room = ensure_direct(deleter, peer)

    owned_group = create_group(deleter, f"{deleter.label} Delete Owned Group {suffix}")
    invite_and_join(deleter, peer, owned_group)
    invite_and_join(deleter, observer, owned_group)

    owned_channel = create_channel(deleter, f"{deleter.label} Delete Owned Channel {suffix}", "private", "invite")
    invite_channel_and_join(deleter, peer, owned_channel)
    invite_channel_and_join(deleter, observer, owned_channel)

    member_group = create_group(peer, f"{peer.label} Delete Member Group {suffix}")
    invite_and_join(peer, deleter, member_group)

    member_channel = create_channel(peer, f"{peer.label} Delete Member Channel {suffix}", "private", "invite")
    invite_channel_and_join(peer, deleter, member_channel)

    deleted = p2p(deleter, "command", "portal.account.delete", {"confirm": "delete_account"})
    expect(deleted.get("status") == "deprovisioned", f"{deleter.label} account delete failed: {deleted!r}")
    expect(int(deleted.get("contacts_left") or 0) >= 1, f"{deleter.label} account delete did not leave contacts: {deleted!r}")
    expect(int(deleted.get("groups_dissolved") or 0) >= 1, f"{deleter.label} account delete did not dissolve owned groups: {deleted!r}")
    expect(int(deleted.get("channels_dissolved") or 0) >= 1, f"{deleter.label} account delete did not dissolve owned channels: {deleted!r}")
    expect(int(deleted.get("groups_left") or 0) >= 1, f"{deleter.label} account delete did not leave member groups: {deleted!r}")
    expect(int(deleted.get("channels_left") or 0) >= 1, f"{deleter.label} account delete did not leave member channels: {deleted!r}")

    wait_until(
        f"{peer.label} still shows deleted peer direct contact",
        lambda: contact_visible(peer, deleter.mxid, direct_room) is None,
        seconds=120,
    )
    for node in (peer, observer):
        wait_until(
            f"{node.label} still shows dissolved owner group",
            lambda n=node: room_removed_from_product_views(n, owned_group, "group"),
            seconds=120,
        )
        wait_until(
            f"{node.label} still shows dissolved owner channel",
            lambda n=node: room_removed_from_product_views(n, owned_channel.get("room_id") or "", "channel"),
            seconds=120,
        )
    wait_until(
        f"{peer.label} group members still show deleted account as joined",
        lambda: joined_member(peer, "groups.members", {"room_id": member_group}, deleter.mxid) is None,
        seconds=120,
    )
    wait_until(
        f"{peer.label} channel members still show deleted account as joined",
        lambda: joined_member(
            peer,
            "channels.members",
            {"room_id": member_channel.get("room_id"), "channel_id": member_channel.get("channel_id")},
            deleter.mxid,
        )
        is None,
        seconds=120,
    )


def assert_mcp_group_tools(nodes: list[Node], room_id: str, suffix: int) -> None:
    for sender in nodes:
        search = mcp(sender, "mcp.rooms.search", {"type": "group", "limit": 100})
        rooms = list(search.get("rooms") or [])
        expect(
            any(room.get("room_id") == room_id and room.get("type") == "group" for room in rooms),
            f"{sender.label} mcp.rooms.search did not return shared group",
        )

        text = f"mcp agent group message {sender.label} {suffix}"
        sent = mcp(sender, "mcp.messages.send", {"room_id": room_id, "msg": text})
        expect(sent.get("ok") is True and sent.get("event_id"), f"{sender.label} mcp.messages.send failed")
        from_time = sent.get("created_at") or ""

        for viewer in nodes:
            wait_until(
                f"{viewer.label} mcp.messages.list did not see {sender.label} message",
                lambda v=viewer, t=text, f=from_time: any(
                    message.get("msg") == t
                    for message in list(
                        mcp(v, "mcp.messages.list", {"room_id": room_id, "from_time": f, "limit": 100}).get("messages")
                        or []
                    )
                ),
            )


def assert_mcp_channel_tools(nodes: list[Node], suffix: int) -> None:
    for owner in nodes:
        channel = p2p(
            owner,
            "command",
            "channels.create",
            {
                "name": f"{owner.label} MCP Channel {suffix}",
                "visibility": "public",
                "join_policy": "open",
                "channel_type": "post",
                "comments_enabled": True,
            },
        )
        channel_id = channel.get("channel_id") or ""
        room_id = channel.get("room_id") or ""
        expect(bool(channel_id and room_id), f"{owner.label} channels.create for MCP did not return channel identifiers")

        post_body = f"mcp post {owner.label} {suffix}"
        post = p2p(
            owner,
            "command",
            "channels.posts.create",
            {"channel_id": channel_id, "room_id": room_id, "body": post_body, "message_type": "text"},
        )
        post_id = post.get("post_id") or ""
        expect(bool(post_id), f"{owner.label} channels.posts.create for MCP did not return post_id")

        posts = mcp(owner, "mcp.channel_posts.list", {"room_id": room_id, "limit": 20})
        expect(
            any(item.get("post_id") == post_id and item.get("msg") == post_body for item in list(posts.get("posts") or [])),
            f"{owner.label} mcp.channel_posts.list did not return created post",
        )

        comment_body = f"mcp comment {owner.label} {suffix}"
        created = mcp(owner, "mcp.channel_comments.create", {"post_id": post_id, "msg": comment_body})
        comment_id = created.get("comment_id") or ""
        expect(created.get("ok") is True and comment_id, f"{owner.label} mcp.channel_comments.create failed")

        comments = mcp(owner, "mcp.channel_comments.list", {"post_id": post_id, "limit": 20})
        expect(
            any(item.get("comment_id") == comment_id and item.get("msg") == comment_body for item in list(comments.get("comments") or [])),
            f"{owner.label} mcp.channel_comments.list did not return created comment",
        )


def assert_remote_post_channel_history(owner: Node, joiner: Node, channel: dict[str, Any], suffix: int) -> None:
    channel_id = channel.get("channel_id") or ""
    room_id = channel.get("room_id") or ""
    expect(bool(channel_id and room_id), "post channel did not include channel_id and room_id")

    first_body = f"historical post one {suffix}"
    second_body = f"historical post two {suffix}"
    first_post = p2p(
        owner,
        "command",
        "channels.posts.create",
        {"channel_id": channel_id, "room_id": room_id, "body": first_body, "message_type": "text"},
    )
    first_post_id = first_post.get("post_id") or ""
    second_post = p2p(
        owner,
        "command",
        "channels.posts.create",
        {"channel_id": channel_id, "room_id": room_id, "body": second_body, "message_type": "text"},
    )
    second_post_id = second_post.get("post_id") or ""
    expect(bool(first_post_id and second_post_id), "historical channel posts were not created")

    comment_body = f"historical comment {suffix}"
    comment = p2p(
        owner,
        "command",
        "channels.comments.create",
        {"channel_id": channel_id, "room_id": room_id, "post_id": first_post_id, "body": comment_body},
    )
    comment_id = comment.get("comment_id") or ""
    expect(bool(comment_id), "historical channel comment was not created")

    post_reaction = p2p(
        owner,
        "command",
        "channels.post_reaction.toggle",
        {"channel_id": channel_id, "room_id": room_id, "post_id": first_post_id, "reaction": "like"},
    )
    expect(post_reaction.get("active") is True, "historical post reaction did not activate")
    comment_reaction = p2p(
        owner,
        "command",
        "channels.comment_reaction.toggle",
        {
            "channel_id": channel_id,
            "room_id": room_id,
            "post_id": first_post_id,
            "comment_id": comment_id,
            "reaction": "like",
        },
    )
    expect(comment_reaction.get("active") is True, "historical comment reaction did not activate")

    owner_p2p_base = owner.base.replace("127.0.0.1", PUBLIC_HOST)
    joiner_p2p_base = joiner.base.replace("127.0.0.1", PUBLIC_HOST)
    joined = p2p(
        joiner,
        "command",
        "channels.public.join_request",
        {
            "room_id": room_id,
            "channel_id": channel_id,
            "remote_node_base_url": f"{owner_p2p_base}/_p2p",
            "requester_node_base_url": f"{joiner_p2p_base}/_p2p",
            "server_names": [owner.server_name],
            "display_name": joiner.name,
            "avatar_url": joiner.avatar,
        },
    )
    expect(joined.get("status") == "joined", f"{joiner.label} did not join remote post channel: {joined!r}")

    def joined_posts_ready():
        posts = list((p2p(joiner, "command", "channels.posts.list", {"channel_id": channel_id}).get("posts") or []))
        first = find_by(posts, post_id=first_post_id)
        second = find_by(posts, post_id=second_post_id)
        if not first or not second:
            return None
        if first.get("body") != first_body or second.get("body") != second_body:
            return None
        if int(first.get("comment_count") or 0) < 1 or int(first.get("reaction_count") or 0) < 1:
            return None
        return posts

    wait_until(f"{joiner.label} product posts list did not backfill historical posts/reactions", joined_posts_ready)

    def joined_comments_ready():
        comments = list((p2p(joiner, "command", "channels.comments.list", {"post_id": first_post_id}).get("comments") or []))
        first_comment = find_by(comments, comment_id=comment_id)
        if not first_comment:
            return None
        if first_comment.get("body") != comment_body or int(first_comment.get("reaction_count") or 0) < 1:
            return None
        return comments

    wait_until(f"{joiner.label} product comments list did not backfill historical comment/reaction", joined_comments_ready)
    assert_matrix_channel_content(joiner, room_id, [first_body, second_body, comment_body])


def main() -> int:
    os.environ.setdefault("P2P_DUAL_PUBLIC_HOST", PUBLIC_HOST)
    suffix = int(time.time() * 1000)
    nodes = [
        Node("A", "http://127.0.0.1:18008", "dirextalk-p2p-dual-dendrite-a-1", f"{PUBLIC_HOST}:18448", "", ""),
        Node("B", "http://127.0.0.1:28008", "dirextalk-p2p-dual-dendrite-b-1", f"{PUBLIC_HOST}:28448", "", ""),
        Node("C", "http://127.0.0.1:38008", "dirextalk-p2p-dual-dendrite-c-1", f"{PUBLIC_HOST}:38448", "", ""),
    ]
    for node in nodes:
        node.mxid = f"@owner:{node.server_name}"
        bootstrap = read_bootstrap(node.container)
        node.password = bootstrap.get("password") or ""
        node.agent_token = bootstrap.get("agent_token") or ""
        expect(bool(node.password), f"{node.label} bootstrap did not include password")
        expect(bool(node.agent_token), f"{node.label} bootstrap did not include agent_token")
        health = request_json("GET", f"{node.base}/_p2p/health")
        expect(health.get("status") == "ok", f"{node.label} health failed")
        login(node)
        update_profile(node, suffix)
        print(f"PASS {node.label} health/login/profile")

    for node in nodes:
        login(node)

    a, b, c = nodes

    verify_rebuilt_peer_readd(a, c, suffix)
    print("PASS C empty-state rebuild re-add restores contact, using old room when joinable or replacement room when not")

    direct_room = ensure_direct(a, b)
    verify_delete_readd(b, a, direct_room)
    verify_mutual_delete_readd(a, b, direct_room, suffix)
    status, _ = p2p_status(a, "command", "contacts.request", {"mxid": a.mxid, "display_name": a.name})
    expect(status == 400, "contacts.request allowed adding self")
    print("PASS direct accepted capabilities, delete/re-add old room, mutual-delete accept sync, self-add guard")

    verify_rebuilt_room_reentry(a, c, suffix)
    print("PASS C empty-state rebuild requires explicit group/private-channel rejoin and public-channel reapply")

    message_group = create_group(b, f"BCA Three Message {suffix}")
    invite_and_join(b, c, message_group)
    invite_and_join(b, a, message_group)
    assert_members(b, message_group, [a, b, c])
    group_text = f"three-node group message {suffix}"
    event_id = wait_matrix_send_text(b, message_group, group_text, "Matrix group send did not return event_id")
    assert_message_projection([a, b, c], message_group, group_text)
    print("PASS group invite C-first/A-second, member nicknames, group preview capabilities")

    assert_mcp_group_tools([a, b, c], message_group, suffix)
    print("PASS MCP Agent token search/send/list across A/B/C shared group")

    a_first_group = create_group(b, f"BAC Three Order {suffix}")
    invite_and_join(b, a, a_first_group)
    invite_and_join(b, c, a_first_group)
    assert_members(b, a_first_group, [a, b, c])
    a_first_conv = wait_until("A conversations missing A-first group", lambda: conversation_for(a, a_first_group))
    c_second_conv = wait_until("C conversations missing C-second group", lambda: conversation_for(c, a_first_group))
    assert_caps(a_first_conv, send=True, send_media=True, call=True, label="A first-order group")
    assert_caps(c_second_conv, send=True, send_media=True, call=True, label="C second-order group")
    print("PASS group invite A-first/C-second, member nicknames, group capabilities")

    empty_group = create_group(b, f"BC Three Empty {suffix}")
    invite_and_join(b, c, empty_group)
    empty_conv = wait_until("C conversations missing joined empty group", lambda: conversation_for(c, empty_group))
    expect(not empty_conv.get("last_message"), "empty group unexpectedly has last_message")
    assert_caps(empty_conv, send=True, send_media=True, call=True, label="C empty group")
    c_group_rooms = {conv.get("matrix_room_id") for conv in conversations(c) if conv.get("kind") == "group"}
    expect({message_group, empty_group}.issubset(c_group_rooms), "C conversations did not retain multiple joined groups")
    print("PASS empty group and multi-group conversation list")

    channel = p2p(
        b,
        "command",
        "channels.create",
        {
            "name": f"B Three Channel {suffix}",
            "visibility": "public",
            "join_policy": "open",
            "channel_type": "post",
            "comments_enabled": True,
        },
    )
    channel_room = channel.get("room_id") or ""
    channel_conv = wait_until("B conversations missing channel", lambda: conversation_for(b, channel_room))
    assert_caps(
        channel_conv,
        send=True,
        send_media=True,
        call=False,
        label="B channel",
        extra={
            "post_create": True,
            "comment_create": True,
            "reaction_toggle": True,
            "comments_enabled": True,
        },
    )
    print("PASS channel capabilities call=false post/comment/reaction=true")

    assert_remote_post_channel_history(b, a, channel, suffix)
    print("PASS remote post channel join backfills historical posts/comments/reactions to product and Matrix clients")

    assert_mcp_channel_tools([a, b, c], suffix)
    print("PASS MCP Agent token channel post/comment tools on A/B/C")

    run(["docker", "restart", c.container])
    wait_until("C did not become healthy after restart", lambda: request_json("GET", f"{c.base}/_p2p/health").get("status") == "ok")
    login(c)
    wait_until("C conversations missing message group after restart", lambda: conversation_for(c, message_group))
    wait_until("C conversations missing empty group after restart", lambda: conversation_for(c, empty_group))
    assert_history(c, message_group, group_text)
    print("PASS C restart persistence and Matrix history load")

    verify_account_delete_deprovision_propagates(b, a, c, suffix)
    print("PASS account deletion leaves direct contacts/member rooms, dissolves owned rooms, and propagates removal to peers")

    print("THREE_NODE_REGRESSION_PASS")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as exc:
        print(f"THREE_NODE_REGRESSION_FAIL: {exc}", file=sys.stderr)
        raise
