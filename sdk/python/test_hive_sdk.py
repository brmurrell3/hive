# SPDX-License-Identifier: Apache-2.0
# Copyright 2025 The Hive Authors

"""Unit tests for the Hive Python SDK.

Run with: python -m pytest test_hive_sdk.py  (or)  python -m unittest test_hive_sdk
"""

from __future__ import annotations

import json
import os
import signal
import threading
import time
import unittest
from http.client import HTTPConnection
from typing import Any, Dict
from unittest import mock

from hive_sdk import HiveAgent, _parse_timeout_seconds


class TestCapabilityRegistration(unittest.TestCase):
    """Test capability registration via the @agent.capability decorator."""

    def test_register_capability(self) -> None:
        agent = HiveAgent(callback_port=0)

        @agent.capability("greet")
        def greet(name: str) -> Dict[str, Any]:
            return {"message": f"Hello, {name}!"}

        self.assertIn("greet", agent.capabilities)
        self.assertIs(agent.capabilities["greet"], greet)

    def test_register_multiple_capabilities(self) -> None:
        agent = HiveAgent(callback_port=0)

        @agent.capability("cap_a")
        def cap_a() -> Dict[str, Any]:
            return {"a": True}

        @agent.capability("cap_b")
        def cap_b() -> Dict[str, Any]:
            return {"b": True}

        self.assertEqual(len(agent.capabilities), 2)
        self.assertIn("cap_a", agent.capabilities)
        self.assertIn("cap_b", agent.capabilities)

    def test_duplicate_capability_raises(self) -> None:
        agent = HiveAgent(callback_port=0)

        @agent.capability("dup")
        def first() -> Dict[str, Any]:
            return {}

        with self.assertRaises(ValueError) as ctx:
            @agent.capability("dup")
            def second() -> Dict[str, Any]:
                return {}

        self.assertIn("already registered", str(ctx.exception))

    def test_empty_name_raises(self) -> None:
        agent = HiveAgent(callback_port=0)
        with self.assertRaises(ValueError):
            @agent.capability("")
            def noop() -> Dict[str, Any]:
                return {}

    def test_decorator_preserves_function(self) -> None:
        agent = HiveAgent(callback_port=0)

        @agent.capability("echo")
        def echo(msg: str = "hi") -> Dict[str, Any]:
            return {"echo": msg}

        # Function should still be callable directly.
        result = echo(msg="test")
        self.assertEqual(result, {"echo": "test"})


class TestEnvironmentVariables(unittest.TestCase):
    """Test that HiveAgent reads configuration from environment variables."""

    @mock.patch.dict(os.environ, {
        "HIVE_AGENT_ID": "test-agent",
        "HIVE_TEAM_ID": "test-team",
        "HIVE_SIDECAR_URL": "http://127.0.0.1:9100",
        "HIVE_CALLBACK_PORT": "9200",
        "HIVE_WORKSPACE": "/tmp/workspace",
    })
    def test_reads_all_env_vars(self) -> None:
        agent = HiveAgent()
        self.assertEqual(agent.agent_id, "test-agent")
        self.assertEqual(agent.team_id, "test-team")
        self.assertEqual(agent.sidecar_url, "http://127.0.0.1:9100")
        self.assertEqual(agent.callback_port, 9200)
        self.assertEqual(agent.workspace, "/tmp/workspace")

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_defaults_when_no_env(self) -> None:
        agent = HiveAgent()
        self.assertEqual(agent.agent_id, "")
        self.assertEqual(agent.team_id, "")
        self.assertEqual(agent.sidecar_url, "")
        self.assertEqual(agent.workspace, "")

    @mock.patch.dict(os.environ, {}, clear=True)
    def test_callback_port_required(self) -> None:
        agent = HiveAgent()
        with self.assertRaises(RuntimeError) as ctx:
            _ = agent.callback_port
        self.assertIn("HIVE_CALLBACK_PORT", str(ctx.exception))

    def test_constructor_overrides_env(self) -> None:
        with mock.patch.dict(os.environ, {"HIVE_AGENT_ID": "env-agent"}):
            agent = HiveAgent(agent_id="override-agent", callback_port=8080)
            self.assertEqual(agent.agent_id, "override-agent")
            self.assertEqual(agent.callback_port, 8080)


class _TestServerMixin:
    """Mixin to start/stop a HiveAgent server for HTTP tests."""

    agent: HiveAgent
    port: int

    def start_server(self) -> None:
        # Find a free port.
        import socket
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]

        self.agent = HiveAgent(
            agent_id="test-agent",
            team_id="test-team",
            callback_port=self.port,
        )

    def serve_in_thread(self) -> None:
        self._server_thread = threading.Thread(
            target=self.agent.run, daemon=True
        )
        self._server_thread.start()
        # Wait for server to be ready.
        self._wait_for_server()

    def _wait_for_server(self, retries: int = 50) -> None:
        for _ in range(retries):
            try:
                conn = HTTPConnection("127.0.0.1", self.port, timeout=1)
                conn.request("GET", "/health")
                resp = conn.getresponse()
                resp.read()
                conn.close()
                if resp.status == 200:
                    return
            except (ConnectionRefusedError, OSError):
                pass
            time.sleep(0.05)
        raise RuntimeError("server did not start in time")

    def stop_server(self) -> None:
        self.agent.stop()
        self._server_thread.join(timeout=5)

    def http_request(
        self, method: str, path: str, body: Any = None
    ) -> tuple:
        """Make an HTTP request and return (status, parsed_json_body)."""
        conn = HTTPConnection("127.0.0.1", self.port, timeout=5)
        headers = {}
        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
            headers["Content-Length"] = str(len(data))
        conn.request(method, path, body=data, headers=headers)
        resp = conn.getresponse()
        raw = resp.read()
        conn.close()
        try:
            parsed = json.loads(raw.decode("utf-8"))
        except (json.JSONDecodeError, UnicodeDecodeError):
            parsed = None
        return resp.status, parsed


class TestHealthEndpoint(unittest.TestCase, _TestServerMixin):
    """Test the GET /health endpoint."""

    def setUp(self) -> None:
        self.start_server()
        self.serve_in_thread()

    def tearDown(self) -> None:
        self.stop_server()

    def test_health_returns_200(self) -> None:
        status, body = self.http_request("GET", "/health")
        self.assertEqual(status, 200)
        self.assertEqual(body, {"status": "healthy"})

    def test_unknown_get_returns_404(self) -> None:
        status, body = self.http_request("GET", "/unknown")
        self.assertEqual(status, 404)


class TestCapabilityHandler(unittest.TestCase, _TestServerMixin):
    """Test the POST /handle/{capability} endpoint."""

    def setUp(self) -> None:
        self.start_server()

        @self.agent.capability("greet")
        def greet(name: str, greeting: str = "Hello") -> Dict[str, Any]:
            return {"message": f"{greeting}, {name}!"}

        @self.agent.capability("add")
        def add(a: int, b: int) -> Dict[str, Any]:
            return {"sum": a + b}

        @self.agent.capability("fail")
        def fail() -> Dict[str, Any]:
            raise ValueError("something went wrong")

        @self.agent.capability("non_dict")
        def non_dict() -> str:
            return "just a string"  # type: ignore[return-value]

        self.serve_in_thread()

    def tearDown(self) -> None:
        self.stop_server()

    def test_invoke_capability(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/greet", {"inputs": {"name": "World"}}
        )
        self.assertEqual(status, 200)
        self.assertEqual(body["outputs"]["message"], "Hello, World!")

    def test_invoke_with_optional_args(self) -> None:
        status, body = self.http_request(
            "POST",
            "/handle/greet",
            {"inputs": {"name": "Alice", "greeting": "Hi"}},
        )
        self.assertEqual(status, 200)
        self.assertEqual(body["outputs"]["message"], "Hi, Alice!")

    def test_invoke_add(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/add", {"inputs": {"a": 3, "b": 4}}
        )
        self.assertEqual(status, 200)
        self.assertEqual(body["outputs"]["sum"], 7)

    def test_unknown_capability_returns_404(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/unknown", {"inputs": {}}
        )
        self.assertEqual(status, 404)
        self.assertIn("error", body)

    def test_missing_path_returns_404(self) -> None:
        status, body = self.http_request("POST", "/other", {"inputs": {}})
        self.assertEqual(status, 404)

    def test_empty_body(self) -> None:
        """POST with no body should pass empty inputs."""
        status, body = self.http_request("POST", "/handle/greet", None)
        # greet requires 'name' so it will fail with TypeError
        self.assertEqual(status, 500)
        self.assertIn("error", body)
        self.assertEqual(body["error"]["code"], "CAPABILITY_FAILED")

    def test_invalid_json_body(self) -> None:
        conn = HTTPConnection("127.0.0.1", self.port, timeout=5)
        conn.request(
            "POST",
            "/handle/greet",
            body=b"not json",
            headers={
                "Content-Type": "application/json",
                "Content-Length": "8",
            },
        )
        resp = conn.getresponse()
        raw = resp.read()
        conn.close()
        self.assertEqual(resp.status, 400)
        body = json.loads(raw)
        self.assertEqual(body["error"]["code"], "INVALID_REQUEST")

    def test_non_dict_inputs(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/greet", {"inputs": "not a dict"}
        )
        self.assertEqual(status, 400)
        self.assertEqual(body["error"]["code"], "INVALID_REQUEST")


class TestErrorHandling(unittest.TestCase, _TestServerMixin):
    """Test that exceptions in handlers are returned as structured errors."""

    def setUp(self) -> None:
        self.start_server()

        @self.agent.capability("fail")
        def fail() -> Dict[str, Any]:
            raise ValueError("something went wrong")

        @self.agent.capability("runtime_error")
        def runtime_error() -> Dict[str, Any]:
            raise RuntimeError("internal error")

        self.serve_in_thread()

    def tearDown(self) -> None:
        self.stop_server()

    def test_exception_returns_500(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/fail", {"inputs": {}}
        )
        self.assertEqual(status, 500)
        self.assertEqual(body["error"]["code"], "CAPABILITY_FAILED")
        self.assertEqual(body["error"]["message"], "something went wrong")

    def test_runtime_error(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/runtime_error", {"inputs": {}}
        )
        self.assertEqual(status, 500)
        self.assertEqual(body["error"]["code"], "CAPABILITY_FAILED")
        self.assertIn("internal error", body["error"]["message"])


class TestNonDictReturn(unittest.TestCase, _TestServerMixin):
    """Test that non-dict returns are wrapped properly."""

    def setUp(self) -> None:
        self.start_server()

        @self.agent.capability("stringy")
        def stringy() -> str:
            return "just a string"  # type: ignore[return-value]

        @self.agent.capability("listy")
        def listy() -> list:
            return [1, 2, 3]  # type: ignore[return-value]

        self.serve_in_thread()

    def tearDown(self) -> None:
        self.stop_server()

    def test_non_dict_wrapped(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/stringy", {"inputs": {}}
        )
        self.assertEqual(status, 200)
        self.assertEqual(body["outputs"]["result"], "just a string")

    def test_list_wrapped(self) -> None:
        status, body = self.http_request(
            "POST", "/handle/listy", {"inputs": {}}
        )
        self.assertEqual(status, 200)
        self.assertEqual(body["outputs"]["result"], [1, 2, 3])


class TestInvokeRemote(unittest.TestCase):
    """Test the agent.invoke() method for remote capability invocation."""

    def test_invoke_requires_sidecar_url(self) -> None:
        agent = HiveAgent(callback_port=0)
        with self.assertRaises(RuntimeError) as ctx:
            agent.invoke("target", "cap", {"key": "val"})
        self.assertIn("HIVE_SIDECAR_URL", str(ctx.exception))

    @mock.patch("hive_sdk.urlopen")
    def test_invoke_success(self, mock_urlopen: mock.MagicMock) -> None:
        mock_resp = mock.MagicMock()
        mock_resp.read.return_value = json.dumps(
            {"status": "ok", "outputs": {"result": 42}}
        ).encode("utf-8")
        mock_resp.__enter__ = mock.MagicMock(return_value=mock_resp)
        mock_resp.__exit__ = mock.MagicMock(return_value=False)
        mock_urlopen.return_value = mock_resp

        agent = HiveAgent(
            sidecar_url="http://127.0.0.1:9100", callback_port=0
        )
        result = agent.invoke("target-agent", "do-thing", {"x": 1})
        self.assertEqual(result["status"], "ok")
        self.assertEqual(result["outputs"]["result"], 42)

        # Verify the request was made correctly.
        call_args = mock_urlopen.call_args
        req = call_args[0][0]
        self.assertIn("/capabilities/do-thing/invoke-remote", req.full_url)
        body = json.loads(req.data.decode("utf-8"))
        self.assertEqual(body["target"], "target-agent")
        self.assertEqual(body["inputs"], {"x": 1})
        self.assertEqual(body["timeout"], "30s")

    @mock.patch("hive_sdk.urlopen")
    def test_invoke_url_error(self, mock_urlopen: mock.MagicMock) -> None:
        from urllib.error import URLError
        mock_urlopen.side_effect = URLError("connection refused")

        agent = HiveAgent(
            sidecar_url="http://127.0.0.1:9100", callback_port=0
        )
        result = agent.invoke("target", "cap", {})
        self.assertEqual(result["status"], "error")
        self.assertEqual(result["error"]["code"], "INVOCATION_FAILED")


class TestParseTimeout(unittest.TestCase):
    """Test the _parse_timeout_seconds helper."""

    def test_seconds(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("30s"), 30.0)

    def test_minutes(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("2m"), 120.0)

    def test_hours(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("1h"), 3600.0)

    def test_milliseconds(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("500ms"), 0.5)

    def test_plain_number(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("45"), 45.0)

    def test_invalid_fallback(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("abc"), 30.0)

    def test_whitespace(self) -> None:
        self.assertAlmostEqual(_parse_timeout_seconds("  10s  "), 10.0)


class TestStopMethod(unittest.TestCase, _TestServerMixin):
    """Test that agent.stop() cleanly shuts down the server."""

    def test_stop_shuts_down_server(self) -> None:
        self.start_server()

        @self.agent.capability("ping")
        def ping() -> Dict[str, Any]:
            return {"pong": True}

        self.serve_in_thread()

        # Verify server is running.
        status, body = self.http_request("GET", "/health")
        self.assertEqual(status, 200)

        # Stop and verify thread exits.
        self.agent.stop()
        self._server_thread.join(timeout=5)
        self.assertFalse(self._server_thread.is_alive())


if __name__ == "__main__":
    unittest.main()
