# SPDX-License-Identifier: Apache-2.0
# Copyright 2025 The Hive Authors

"""Hive Python SDK - Single-file agent SDK for the Hive framework.

Provides the HiveAgent class for building agents that expose capabilities
over HTTP. The sidecar calls POST /handle/{capability_name} with JSON
inputs, and the agent returns JSON outputs.

Example usage::

    from hive_sdk import HiveAgent

    agent = HiveAgent()

    @agent.capability("greet")
    def greet(name: str, greeting: str = "Hello"):
        return {"message": f"{greeting}, {name}!"}

    agent.run()

Zero external dependencies - stdlib only.
"""

from __future__ import annotations

import json
import logging
import os
import signal
import sys
import threading
from http.server import HTTPServer, BaseHTTPRequestHandler
from typing import Any, Callable, Dict, Optional
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

__all__ = ["HiveAgent"]
__version__ = "0.1.0"

logger = logging.getLogger("hive_sdk")


class HiveAgent:
    """A Hive agent that exposes capabilities over HTTP.

    Reads configuration from environment variables:
        HIVE_AGENT_ID    - Agent identifier
        HIVE_TEAM_ID     - Team identifier
        HIVE_SIDECAR_URL - Sidecar API URL (e.g. "http://127.0.0.1:9100")
        HIVE_CALLBACK_PORT - Port for the HTTP callback server (required)
        HIVE_WORKSPACE   - Workspace directory

    Capabilities are registered via the ``@agent.capability("name")`` decorator.
    Call ``agent.run()`` to start the HTTP server and block until SIGTERM.
    """

    def __init__(
        self,
        agent_id: Optional[str] = None,
        team_id: Optional[str] = None,
        sidecar_url: Optional[str] = None,
        callback_port: Optional[int] = None,
        workspace: Optional[str] = None,
    ) -> None:
        self.agent_id: str = agent_id or os.environ.get("HIVE_AGENT_ID", "")
        self.team_id: str = team_id or os.environ.get("HIVE_TEAM_ID", "")
        self.sidecar_url: str = sidecar_url or os.environ.get("HIVE_SIDECAR_URL", "")
        self.workspace: str = workspace or os.environ.get("HIVE_WORKSPACE", "")

        port_str = os.environ.get("HIVE_CALLBACK_PORT", "")
        if callback_port is not None:
            self._callback_port: Optional[int] = callback_port
        elif port_str:
            self._callback_port = int(port_str)
        else:
            self._callback_port = None

        self._capabilities: Dict[str, Callable[..., Any]] = {}
        self._server: Optional[HTTPServer] = None
        self._shutdown_event = threading.Event()

    @property
    def callback_port(self) -> int:
        """Return the callback port, raising if not configured."""
        if self._callback_port is None:
            raise RuntimeError(
                "HIVE_CALLBACK_PORT environment variable is required but not set"
            )
        return self._callback_port

    def capability(self, name: str) -> Callable:
        """Decorator to register a capability handler.

        The decorated function receives keyword arguments matching the
        capability's input names. It should return a dict that will be
        wrapped as ``{"outputs": {...}}``.

        Example::

            @agent.capability("greet")
            def greet(name: str, greeting: str = "Hello"):
                return {"message": f"{greeting}, {name}!"}

        Args:
            name: The capability name used in the HTTP route.

        Returns:
            Decorator function.

        Raises:
            ValueError: If a capability with the same name is already registered.
        """
        if not name:
            raise ValueError("capability name must not be empty")

        def decorator(func: Callable[..., Any]) -> Callable[..., Any]:
            if name in self._capabilities:
                raise ValueError(
                    f"capability {name!r} is already registered"
                )
            self._capabilities[name] = func
            return func

        return decorator

    def invoke(
        self,
        target: str,
        capability: str,
        inputs: Optional[Dict[str, Any]] = None,
        timeout: str = "30s",
    ) -> Dict[str, Any]:
        """Invoke a capability on a remote agent via the sidecar.

        Sends a POST request to the sidecar's invoke-remote endpoint
        which forwards the request to the target agent over NATS.

        Args:
            target: Target agent ID.
            capability: Capability name to invoke.
            inputs: Input parameters for the capability.
            timeout: Timeout string (e.g. "30s", "1m").

        Returns:
            Response dict from the remote agent.

        Raises:
            RuntimeError: If HIVE_SIDECAR_URL is not configured.
        """
        if not self.sidecar_url:
            raise RuntimeError(
                "HIVE_SIDECAR_URL must be set to invoke remote capabilities"
            )

        url = f"{self.sidecar_url}/capabilities/{capability}/invoke-remote"
        payload = {
            "target": target,
            "inputs": inputs or {},
            "timeout": timeout,
        }
        data = json.dumps(payload).encode("utf-8")
        req = Request(
            url,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )

        try:
            with urlopen(req, timeout=_parse_timeout_seconds(timeout) + 5) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except HTTPError as e:
            try:
                body = json.loads(e.read().decode("utf-8"))
            except (json.JSONDecodeError, AttributeError):
                body = {}
            return {
                "status": "error",
                "error": body.get("error", {
                    "code": "INVOCATION_FAILED",
                    "message": str(e),
                }),
            }
        except URLError as e:
            return {
                "status": "error",
                "error": {
                    "code": "INVOCATION_FAILED",
                    "message": str(e),
                },
            }

    def run(self) -> None:
        """Start the HTTP server and block until SIGTERM or SIGINT.

        The server listens on ``127.0.0.1:{HIVE_CALLBACK_PORT}`` and
        handles capability invocations and health checks.

        Raises:
            RuntimeError: If HIVE_CALLBACK_PORT is not configured.
        """
        port = self.callback_port
        agent = self

        class _Handler(BaseHTTPRequestHandler):
            """HTTP request handler for Hive agent capabilities."""

            def log_message(self, format: str, *args: Any) -> None:
                logger.debug(format, *args)

            def do_GET(self) -> None:
                if self.path == "/health":
                    self._send_json(200, {"status": "healthy"})
                else:
                    self._send_json(404, {"error": "not found"})

            def do_POST(self) -> None:
                # Expected path: /handle/{capability_name}
                prefix = "/handle/"
                if not self.path.startswith(prefix):
                    self._send_json(404, {"error": "not found"})
                    return

                cap_name = self.path[len(prefix):]
                # Strip trailing slash or query params
                if "/" in cap_name:
                    cap_name = cap_name.split("/")[0]
                if "?" in cap_name:
                    cap_name = cap_name.split("?")[0]

                handler = agent._capabilities.get(cap_name)
                if handler is None:
                    self._send_json(
                        404,
                        {
                            "error": {
                                "code": "CAPABILITY_NOT_FOUND",
                                "message": f"capability {cap_name!r} not registered",
                            }
                        },
                    )
                    return

                # Read and parse request body.
                content_length = int(self.headers.get("Content-Length", 0))
                body = {}
                if content_length > 0:
                    raw = self.rfile.read(content_length)
                    try:
                        body = json.loads(raw.decode("utf-8"))
                    except (json.JSONDecodeError, UnicodeDecodeError):
                        self._send_json(
                            400,
                            {
                                "error": {
                                    "code": "INVALID_REQUEST",
                                    "message": "invalid JSON in request body",
                                }
                            },
                        )
                        return

                inputs = body.get("inputs", {})
                if not isinstance(inputs, dict):
                    self._send_json(
                        400,
                        {
                            "error": {
                                "code": "INVALID_REQUEST",
                                "message": "'inputs' must be an object",
                            }
                        },
                    )
                    return

                # Invoke the capability handler.
                try:
                    result = handler(**inputs)
                    if not isinstance(result, dict):
                        result = {"result": result}
                    self._send_json(200, {"outputs": result})
                except Exception as exc:
                    logger.exception(
                        "capability %r raised an exception", cap_name
                    )
                    self._send_json(
                        500,
                        {
                            "error": {
                                "code": "CAPABILITY_FAILED",
                                "message": str(exc),
                            }
                        },
                    )

            def _send_json(self, status: int, data: Any) -> None:
                body = json.dumps(data).encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        HTTPServer.allow_reuse_address = True
        self._server = HTTPServer(("127.0.0.1", port), _Handler)

        def _shutdown_handler(signum: int, frame: Any) -> None:
            logger.info("received signal %d, shutting down", signum)
            self._shutdown_event.set()
            # Shut down in a separate thread to avoid deadlock.
            threading.Thread(
                target=self._server.shutdown, daemon=True
            ).start()

        # Signal handlers can only be registered from the main thread.
        # When run() is called from a background thread (e.g. in tests),
        # skip signal registration gracefully.
        if threading.current_thread() is threading.main_thread():
            signal.signal(signal.SIGTERM, _shutdown_handler)
            signal.signal(signal.SIGINT, _shutdown_handler)

        logger.info(
            "agent %s listening on 127.0.0.1:%d (capabilities: %s)",
            self.agent_id or "<unset>",
            port,
            ", ".join(sorted(self._capabilities)) or "<none>",
        )
        print(
            f"[hive-sdk] Agent {self.agent_id or '<unset>'} listening on "
            f"127.0.0.1:{port}",
            file=sys.stderr,
        )

        self._server.serve_forever()

    def stop(self) -> None:
        """Stop the HTTP server if running."""
        if self._server is not None:
            self._server.shutdown()

    @property
    def capabilities(self) -> Dict[str, Callable[..., Any]]:
        """Return a copy of the registered capabilities dict."""
        return dict(self._capabilities)


def _parse_timeout_seconds(timeout_str: str) -> float:
    """Parse a Go-style duration string into seconds.

    Supports suffixes: s (seconds), m (minutes), h (hours), ms (milliseconds).
    Falls back to 30 seconds if parsing fails.
    """
    s = timeout_str.strip()
    try:
        if s.endswith("ms"):
            return float(s[:-2]) / 1000.0
        elif s.endswith("h"):
            return float(s[:-1]) * 3600.0
        elif s.endswith("m"):
            return float(s[:-1]) * 60.0
        elif s.endswith("s"):
            return float(s[:-1])
        else:
            return float(s)
    except (ValueError, IndexError):
        return 30.0
