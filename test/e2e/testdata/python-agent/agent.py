#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Minimal Python agent using the Hive SDK for integration testing.

from hive_sdk import HiveAgent

agent = HiveAgent()


@agent.capability("echo")
def echo(message=""):
    """Echo back the input message with a prefix."""
    return {"reply": f"echo: {message}"}


@agent.capability("add")
def add(a=0, b=0):
    """Add two numbers."""
    return {"result": int(a) + int(b)}


agent.run()
