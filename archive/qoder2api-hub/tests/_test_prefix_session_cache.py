"""Memory/Redis session mapping is scoped, atomic, and safe on failure."""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *  # noqa: F401,F403

sys.path.insert(0, ROOT)
import qoder_session_cache as C
P, A = bootstrap()

declare_suite(__file__, ["qoder_session_cache.py", "qoder_proxy.py"])


class FakeRedis:
    def __init__(self):
        self.data = {}
        self.calls = []

    def execute(self, *args):
        self.calls.append(args)
        command = args[0]
        if command == "EVAL":
            count = int(args[2])
            keys = args[3:3 + count]
            candidate, ttl = args[3 + count:]
            assert ttl == 3600
            chosen = next((self.data[key] for key in reversed(keys)
                           if key in self.data), candidate)
            self.data[keys[0]] = self.data[keys[-1]] = chosen
            return chosen
        if command == "MGET":
            return [self.data.get(key) for key in args[1:]]
        if command == "GET":
            return self.data.get(args[1])
        if command == "SET":
            _, key, value, nx, ex, ttl = args
            assert nx == "NX" and ex == "EX" and ttl == 3600
            if key in self.data:
                return None
            self.data[key] = value
            return "OK"
        raise AssertionError(args)


cache = C.PrefixSessionCache("redis://localhost:6379/0", hash_secret="test-secret")
cache.redis = FakeRedis()
head = [{"role": "system", "content": "private synthetic system"},
        {"role": "user", "content": "private synthetic first question"}]
tail = head + [{"role": "assistant", "content": "OK"},
               {"role": "user", "content": "second question"}]
first = cache.resolve(head, "account-a", "intl", "ultimate", "client-1")
second = cache.resolve(tail, "account-a", "intl", "ultimate", "client-1")
third = cache.resolve(tail + [{"role": "assistant", "content": "more"}],
                      "account-a", "intl", "ultimate", "client-1")
check("growing conversation reuses the original upstream session",
      first == second == third and len(first) == 36)
check("only hashes and UUIDs are stored in Redis",
      all("private" not in key and "client-1" not in key
          for key in cache.redis.data)
      and all("private" not in value for value in cache.redis.data.values()))
check("different client conversation does not share upstream session",
      cache.resolve(head, "account-a", "intl", "ultimate", "client-2")
      != first)
check("different account does not share upstream session",
      cache.resolve(head, "account-b", "intl", "ultimate", "client-1")
      != first)
check("different model or realm does not share upstream session",
      cache.resolve(head, "account-a", "cn", "ultimate", "client-1")
      != first and cache.resolve(head, "account-a", "intl", "fast",
                                 "client-1") != first)
check("system-only conversation does not create a mapping",
      cache.resolve(head[:1], "account-a", "intl", "ultimate") is None)
check("user-only conversation can create a mapping",
      cache.resolve(head[1:], "account-a", "intl", "ultimate") is not None)
check("different API keys do not share upstream sessions",
      cache.resolve(head, "account-a", "intl", "ultimate", "client-1",
                    tenant_scope="api-key-2") != first)
check("different tool or parameter context does not share upstream session",
      cache.resolve(head, "account-a", "intl", "ultimate", "client-1",
                    context={"tools": ["different"]}) != first)


class RacingRedis(FakeRedis):
    def execute(self, *args):
        if args[0] == "EVAL":
            self.data[args[3]] = "49eb7237-af62-4cc7-85cc-7f112f249b9f"
        return super().execute(*args)


race = C.PrefixSessionCache("redis://localhost:6379/0", hash_secret="test-secret")
race.redis = RacingRedis()
check("atomic assignment preserves the concurrent creator's session",
      race.resolve(head, "a", "intl", "ultimate")
      == "49eb7237-af62-4cc7-85cc-7f112f249b9f")
keys = cache.keys(tail, "account-a", "intl", "ultimate", "client-1")
cache.redis.data[keys[-1]] = "49eb7237-af62-4cc7-85cc-7f112f249b9f"
check("longest existing message prefix wins over shorter anchor",
      cache.resolve(tail, "account-a", "intl", "ultimate", "client-1")
      == "49eb7237-af62-4cc7-85cc-7f112f249b9f")
check("all Redis keys use the same hash slot within one scope",
      len({key.split("{")[1].split("}")[0] for key in keys}) == 1)


def parse(raw):
    import io
    return C.RedisClient._read(io.BytesIO(raw))


check("RESP2 array and nil parse correctly",
      parse(b"*3\r\n$1\r\na\r\n$-1\r\n:7\r\n") == ["a", None, 7])
check("Redis command encoding uses exact argument boundaries",
      C.RedisClient._encode("SET", "key", "value", "NX", "EX", 60)
      == (b"*6\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n"
          b"$2\r\nNX\r\n$2\r\nEX\r\n$2\r\n60\r\n"))

from types import SimpleNamespace
from unittest.mock import patch

account = SimpleNamespace(uid="acct", realm="intl")
body = {"session_id": "wrong-account-session", "messages": head}
context = {"tenant_scope": "key-1"}
with patch.object(P, "prefix_session_cache", return_value=None):
    P.assign_prefix_session(body, account, "ultimate", usage_ctx=context)
check("disabled Redis keeps random behavior and resets stale account session",
      body["session_id"] != "wrong-account-session"
      and context["session_affinity"] == "random")
before = body["session_id"]
with patch.object(P, "prefix_session_cache", side_effect=OSError("secret-url")):
    with patch.object(P, "log") as logger:
        P.assign_prefix_session(body, account, "ultimate", usage_ctx=context)
check("Redis failure does not fail request or leak exception details",
      body["session_id"] != before
      and "secret-url" not in str(logger.call_args))
with patch.object(P, "prefix_session_cache", return_value=cache):
    P.assign_prefix_session(body, account, "ultimate", usage_ctx=context)
check("gateway uses Redis ID and records routing mode",
      context["session_affinity"] == "redis-prefix"
      and len(body["session_id"]) == 36)

# The dependency-free backend follows the same scope/prefix rules.
memory = C.MemorySessionCache(hash_secret="test-secret", max_entries=4)
with patch.object(C.time, "monotonic", return_value=100):
    mem_first = memory.resolve(head, "a", "intl", "ultimate", "client")
with patch.object(C.time, "monotonic", return_value=200):
    mem_second = memory.resolve(tail, "a", "intl", "ultimate", "client")
check("memory reuses growing prefix and refreshes expiry", mem_first == mem_second)
with patch.object(C.time, "monotonic", return_value=3750):
    check("memory hits remain valid after original expiry",
          memory.resolve(tail, "a", "intl", "ultimate", "client") == mem_first)
with patch.object(C.time, "monotonic", return_value=7400):
    check("memory expiry produces a fresh session",
          memory.resolve(tail, "a", "intl", "ultimate", "client") != mem_first)

memory = C.MemorySessionCache(hash_secret="test-secret", max_entries=4)
mem_first = memory.resolve(head, "a", "intl", "ultimate", "client", "key")
for label, args in [
        ("account", ("b", "intl", "ultimate", "client", "key")),
        ("realm", ("a", "cn", "ultimate", "client", "key")),
        ("model", ("a", "intl", "fast", "client", "key")),
        ("conversation", ("a", "intl", "ultimate", "other", "key")),
        ("API key", ("a", "intl", "ultimate", "client", "other"))]:
    check("memory isolates " + label, memory.resolve(head, *args) != mem_first)
check("memory bounds retained entries", len(memory._entries) <= 4)
check("evicted memory prefix receives a fresh UUID",
      memory.resolve(head, "a", "intl", "ultimate", "client", "key") != mem_first)
import concurrent.futures
with concurrent.futures.ThreadPoolExecutor(max_workers=12) as executor:
    ids = list(executor.map(
        lambda _: memory.resolve(tail, "a", "intl", "ultimate", "shared"), range(24)))
check("memory concurrent creators agree on one session", len(set(ids)) == 1)
keys = memory.keys(tail, "a", "intl", "ultimate", "shared")
memory._entries[keys[0]] = ("49eb7237-af62-4cc7-85cc-7f112f249b9f", time.monotonic() + 3600)
check("memory chooses longest prefix over changed anchor",
      memory.resolve(tail, "a", "intl", "ultimate", "shared") == ids[0])
check("memory does not retain message text",
      "private synthetic" not in repr(memory._entries))

with patch.dict(os.environ, {}, clear=True):
    check("no config leaves session affinity off", P.prefix_session_cache() is None)
    os.environ["QD_SESSION_CACHE"] = "memory"
    check("memory backend needs no Redis URL",
          isinstance(P.prefix_session_cache(), C.MemorySessionCache))
    os.environ["QD_SESSION_CACHE"] = "off"
    os.environ["QD_SESSION_REDIS_URL"] = "redis://localhost:6379/0"
    check("explicit off overrides a Redis URL", P.prefix_session_cache() is None)
    del os.environ["QD_SESSION_CACHE"]
    check("Redis URL alone enables Redis for existing configs",
          P.prefix_session_cache().backend == "redis")

# Check the actual serialized request at the shared outbound boundary.
from unittest.mock import Mock
seen = []
acc_a = A.Account({"uid": "a", "realm": "cn", "accessToken": "synthetic"})
acc_b = A.Account({"uid": "b", "realm": "cn", "accessToken": "synthetic"})
session = SimpleNamespace(headers=lambda *args, **kwargs: {})
def capture(req, timeout=None):
    seen.append(json.loads(req.data))
    return Mock()

wire_cache = C.MemorySessionCache(hash_secret="wire-test")
with patch.object(P, "prefix_session_cache", return_value=wire_cache), \
        patch.object(P, "SESSIONS", SimpleNamespace(get=lambda acc: session)), \
        patch.object(P, "qoder_encode", side_effect=lambda b: b.decode()), \
        patch.object(P, "validate_public_http_url", side_effect=lambda url: url), \
        patch.object(P.urllib.request, "urlopen", side_effect=capture):
    wire_ctx = {"tenant_scope": "key-a"}
    for acc, msgs, tenant, conversation in [
            (acc_a, head, "key-a", "client"),
            (acc_a, tail, "key-a", "client"),
            (acc_b, tail, "key-a", "client"),
            (acc_a, tail, "key-b", "client"),
            (acc_a, tail, "key-a", "other")]:
        wire_ctx["tenant_scope"] = tenant
        P.open_upstream({"model": "auto", "messages": msgs},
                        session_key=conversation, target_realm="cn",
                        only_account=acc, usage_ctx=wire_ctx)
check("actual outbound body reuses session for growing history",
      seen[0]["session_id"] == seen[1]["session_id"])
check("actual outbound body isolates accounts, API keys and client IDs",
      len({r["session_id"] for r in [seen[0]] + seen[2:]}) == 4)
check("usage trace matches the actual outgoing session",
      wire_ctx["upstream_session_id"] == seen[-1]["session_id"]
      and wire_ctx["session_affinity"] == "memory-prefix")
check("request IDs remain unique even when session is reused",
      len({r["request_id"] for r in seen}) == len(seen))

# Non-stream retries must carry the authenticated scope rather than silently
# falling into the anonymous namespace after a transport/envelope failure.
for failure in (P.UpstreamStatus(418, "temporary"), ConnectionResetError("reset")):
    with patch.object(P, "aggregate_stream", side_effect=[failure, {"ok": True}]), \
            patch.object(P, "open_upstream", return_value=(Mock(), acc_a, "")) as reopen, \
            patch.object(P, "_handle_envelope_account_cooldown"), \
            patch.object(P.time, "sleep"):
        P.aggregate_with_envelope_retry(Mock(), {}, "client", "cn", "auto",
                                        {}, acc_a, usage_ctx=wire_ctx)
    check("non-stream retry retains tenant scope: " + type(failure).__name__,
          reopen.call_args.kwargs.get("usage_ctx") is wire_ctx)

with patch.object(P, "_persist_usage") as persist:
    P.record_usage("auto", {"prompt_tokens": 100}, session_trace=dict(
        wire_ctx, private_prompt="must-not-persist"))
row = persist.call_args.args[0]
check("usage records only routing fields from session context",
      row.get("upstream_session_id") == wire_ctx["upstream_session_id"]
      and "tenant_scope" not in row and "private_prompt" not in row)

redis_url = os.environ.get("QD_TEST_SESSION_REDIS_URL")
if redis_url:
    import concurrent.futures
    import uuid

    real = C.PrefixSessionCache(redis_url, ttl=3600,
                                hash_secret="integration-private-secret")
    integration_scope = "test-" + uuid.uuid4().hex
    tracked_keys = set()
    try:
        def resolve_actual(_):
            # Independent client instances emulate separate gateway workers.
            client = C.PrefixSessionCache(redis_url, ttl=3600,
                                          hash_secret="integration-private-secret")
            return client.resolve(tail, "test-account", "intl", "ultimate",
                                  integration_scope)

        tracked_keys.update(real.keys(tail, "test-account", "intl", "ultimate",
                                      integration_scope))
        with concurrent.futures.ThreadPoolExecutor(max_workers=12) as executor:
            ids = list(executor.map(resolve_actual, range(24)))
        check("real Redis gives one session to 24 concurrent callers",
              len(set(ids)) == 1)
        live_id = ids[0]
        fresh_client = C.PrefixSessionCache(redis_url, ttl=3600,
                                            hash_secret="integration-private-secret")
        check("separate Redis client reuses the persisted session",
              fresh_client.resolve(tail, "test-account", "intl", "ultimate",
                                   integration_scope) == live_id)
        keys = real.keys(tail, "test-account", "intl", "ultimate", integration_scope)
        check("real Redis applies a bounded TTL",
              0 < real.redis.execute("TTL", keys[0]) <= 3600)
        anchor_id = str(uuid.uuid4())
        real.redis.execute("SET", keys[0], anchor_id, "EX", 3600)
        check("real longest-prefix match overrides an older short anchor",
              real.resolve(tail, "test-account", "intl", "ultimate",
                           integration_scope) == live_id)
        real.redis.execute("DEL", *keys)
        check("expired or removed mappings produce a new UUID",
              real.resolve(tail, "test-account", "intl", "ultimate",
                           integration_scope) != live_id)
    finally:
        if tracked_keys:
            real.redis.execute("DEL", *tracked_keys)
else:
    skip("real Redis integration", "set QD_TEST_SESSION_REDIS_URL to a test endpoint")

finish()
