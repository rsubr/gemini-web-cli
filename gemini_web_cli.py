#!/usr/bin/env python3
"""A small REPL and ACP server that talks to Gemini through an existing Chrome CDP session."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import uuid
from dataclasses import dataclass
from typing import Any

from playwright.async_api import Browser, BrowserContext, Page, async_playwright


GEMINI_URL = "https://gemini.google.com/app"
LOGIN_URL_PARTS = ("accounts.google.com", "signin")
PROMPT_SELECTORS = (
    "textarea",
    "[contenteditable='true'][role='textbox']",
    "[contenteditable='true']",
    "[role='textbox']",
)
RESPONSE_SELECTORS = (
    "model-response",
    "[data-message-author-role='assistant']",
    "[data-testid='assistant-message']",
)


class LoginRequired(RuntimeError):
    """Raised when Gemini cannot be used without signing in."""


@dataclass
class ACPChatSession:
    page: Page
    lock: asyncio.Lock


def page_requires_login(page: Page) -> bool:
    return any(part in page.url.lower() for part in LOGIN_URL_PARTS)


async def ensure_anonymous_access(page: Page) -> None:
    if page_requires_login(page):
        raise LoginRequired("Gemini redirected to Google sign-in.")
    body_text = (await page.locator("body").inner_text()).lower()
    gate_phrases = (
        "sign in to continue",
        "sign in to use gemini",
        "you need to sign in",
        "login to continue",
    )
    if any(phrase in body_text for phrase in gate_phrases):
        raise LoginRequired("Gemini requires sign-in for this session.")


async def prompt_box(page: Page):
    for selector in PROMPT_SELECTORS:
        locator = page.locator(selector).last
        if await locator.count() and await locator.is_visible():
            return locator
    raise RuntimeError("Could not find Gemini's prompt box; its page structure may have changed.")


async def last_response_text(page: Page) -> str:
    for selector in RESPONSE_SELECTORS:
        locator = page.locator(selector)
        if await locator.count():
            text = (await locator.last.inner_text()).strip()
            if text:
                return text
    return ""


async def wait_for_response(page: Page, previous: str, timeout_seconds: float = 120) -> str:
    deadline = time.monotonic() + timeout_seconds
    answer = ""
    unchanged_since: float | None = None
    while time.monotonic() < deadline:
        await ensure_anonymous_access(page)
        current = await last_response_text(page)
        if current and current != previous:
            if current != answer:
                answer = current
                unchanged_since = time.monotonic()
            elif unchanged_since and time.monotonic() - unchanged_since >= 1.5:
                return answer
        await page.wait_for_timeout(250)
    if answer:
        return answer
    raise TimeoutError("Timed out waiting for Gemini's response.")


async def send_prompt(page: Page, prompt: str) -> str:
    await ensure_anonymous_access(page)
    previous = await last_response_text(page)
    box = await prompt_box(page)
    await box.click()
    await page.keyboard.insert_text(prompt)
    await page.keyboard.press("Enter")
    return await wait_for_response(page, previous)


async def new_chat(page: Page) -> None:
    await page.goto(GEMINI_URL, wait_until="domcontentloaded")
    await page.wait_for_timeout(500)
    await ensure_anonymous_access(page)
    await prompt_box(page)


async def choose_context(browser: Browser) -> BrowserContext:
    if not browser.contexts:
        raise RuntimeError("The CDP browser has no available browser context.")
    return browser.contexts[0]


async def repl(cdp_url: str) -> int:
    async with async_playwright() as playwright:
        browser = await playwright.chromium.connect_over_cdp(cdp_url)
        context = await choose_context(browser)
        page = await context.new_page()
        try:
            try:
                await new_chat(page)
            except LoginRequired as exc:
                print(f"Cannot continue anonymously: {exc}", file=sys.stderr)
                return 2
            print("Connected. Commands: /new, /exit")
            while True:
                try:
                    line = await asyncio.to_thread(input, "you> ")
                except (EOFError, KeyboardInterrupt):
                    print()
                    break
                prompt = line.strip()
                if not prompt:
                    continue
                if prompt == "/exit":
                    break
                if prompt == "/new":
                    await new_chat(page)
                    print("Started a new chat.")
                    continue
                if prompt.startswith("/"):
                    print("Unknown command. Commands: /new, /exit")
                    continue
                try:
                    print(f"gemini> {await send_prompt(page, line)}")
                except LoginRequired as exc:
                    print(f"Cannot continue anonymously: {exc}", file=sys.stderr)
                    return 2
                except (RuntimeError, TimeoutError) as exc:
                    print(f"Error: {exc}", file=sys.stderr)
        finally:
            await page.close()
            # Never close browser: it belongs to the user and was attached via CDP.
    return 0


class ACPServer:
    """Minimal ACP v1 server using newline-delimited JSON-RPC on stdio."""

    def __init__(self, cdp_url: str) -> None:
        self.cdp_url = cdp_url
        self.playwright: Any = None
        self.browser: Browser | None = None
        self.sessions: dict[str, ACPChatSession] = {}
        self.turns: dict[str, asyncio.Task[Any]] = {}
        self.write_lock = asyncio.Lock()

    async def send(self, message: dict[str, Any]) -> None:
        async with self.write_lock:
            sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    async def respond(self, request_id: Any, result: Any) -> None:
        await self.send({"jsonrpc": "2.0", "id": request_id, "result": result})

    async def error(self, request_id: Any, code: int, message: str) -> None:
        await self.send({"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}})

    async def update_text(self, session_id: str, text: str) -> None:
        await self.send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": session_id, "update": {"sessionUpdate": "agent_message_chunk", "messageId": f"msg_{uuid.uuid4().hex}", "content": {"type": "text", "text": text}}}})

    async def browser_context(self) -> BrowserContext:
        if self.browser is None:
            self.playwright = await async_playwright().start()
            self.browser = await self.playwright.chromium.connect_over_cdp(self.cdp_url)
        return await choose_context(self.browser)

    async def create_session(self) -> str:
        page = await (await self.browser_context()).new_page()
        try:
            await new_chat(page)
        except Exception:
            await page.close()
            raise
        session_id = f"gemini_{uuid.uuid4().hex}"
        self.sessions[session_id] = ACPChatSession(page, asyncio.Lock())
        return session_id

    @staticmethod
    def prompt_text(blocks: Any) -> str:
        if not isinstance(blocks, list):
            raise ValueError("session/prompt requires a prompt array")
        parts: list[str] = []
        for block in blocks:
            if not isinstance(block, dict):
                continue
            if block.get("type") == "text" and isinstance(block.get("text"), str):
                parts.append(block["text"])
            elif block.get("type") == "resource" and isinstance(block.get("resource"), dict):
                text = block["resource"].get("text")
                if isinstance(text, str):
                    parts.append(text)
        prompt = "\n\n".join(parts).strip()
        if not prompt:
            raise ValueError("The prompt did not contain text Gemini can receive")
        return prompt

    async def close_session(self, session_id: str) -> bool:
        session = self.sessions.pop(session_id, None)
        if session is None:
            return False
        turn = self.turns.get(session_id)
        if turn and not turn.done():
            turn.cancel()
        await session.page.close()
        return True

    async def handle_prompt(self, request_id: Any, params: dict[str, Any]) -> None:
        session_id = params.get("sessionId")
        session = self.sessions.get(session_id)
        if session is None:
            await self.error(request_id, -32002, "Unknown ACP session")
            return
        try:
            prompt = self.prompt_text(params.get("prompt"))
        except ValueError as exc:
            await self.error(request_id, -32602, str(exc))
            return
        task = asyncio.current_task()
        if task:
            self.turns[session_id] = task
        try:
            async with session.lock:
                answer = await send_prompt(session.page, prompt)
            await self.update_text(session_id, answer)
            await self.respond(request_id, {"stopReason": "end_turn"})
        except asyncio.CancelledError:
            await self.respond(request_id, {"stopReason": "cancelled"})
        except LoginRequired as exc:
            await self.update_text(session_id, f"Cannot continue anonymously: {exc}")
            await self.respond(request_id, {"stopReason": "refusal"})
        except (RuntimeError, TimeoutError) as exc:
            await self.update_text(session_id, f"Gemini error: {exc}")
            await self.respond(request_id, {"stopReason": "refusal"})
        finally:
            if self.turns.get(session_id) is task:
                self.turns.pop(session_id, None)

    async def handle(self, message: dict[str, Any]) -> None:
        request_id = message.get("id")
        method = message.get("method")
        params = message.get("params") or {}
        if not isinstance(params, dict):
            if request_id is not None:
                await self.error(request_id, -32602, "params must be an object")
            return
        if method == "initialize":
            await self.respond(request_id, {"protocolVersion": 1, "agentCapabilities": {"promptCapabilities": {}}, "agentInfo": {"name": "gemini-web-cli", "title": "Gemini Web CLI", "version": "0.1.0"}, "authMethods": []})
        elif method == "session/new":
            try:
                await self.respond(request_id, {"sessionId": await self.create_session()})
            except Exception as exc:
                await self.error(request_id, -32000, f"Could not create Gemini session: {exc}")
        elif method == "session/prompt":
            await self.handle_prompt(request_id, params)
        elif method == "session/cancel":
            turn = self.turns.get(params.get("sessionId"))
            if turn and not turn.done():
                turn.cancel()
        elif method == "session/close":
            if await self.close_session(params.get("sessionId")):
                await self.respond(request_id, {})
            else:
                await self.error(request_id, -32002, "Unknown ACP session")
        elif request_id is not None:
            await self.error(request_id, -32601, f"Unsupported ACP method: {method}")

    async def run(self) -> int:
        tasks: set[asyncio.Task[Any]] = set()
        try:
            while line := await asyncio.to_thread(sys.stdin.buffer.readline):
                try:
                    message = json.loads(line)
                except json.JSONDecodeError as exc:
                    await self.error(None, -32700, f"Parse error: {exc.msg}")
                    continue
                if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
                    await self.error(message.get("id") if isinstance(message, dict) else None, -32600, "Invalid JSON-RPC request")
                    continue
                task = asyncio.create_task(self.handle(message))
                tasks.add(task)
                task.add_done_callback(tasks.discard)
        finally:
            if tasks:
                await asyncio.gather(*tasks, return_exceptions=True)
            for session_id in list(self.sessions):
                await self.close_session(session_id)
            if self.playwright is not None:
                await self.playwright.stop()
        return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cdp-url", default=os.environ.get("CHROME_CDP_URL", "http://localhost:9222"), help="Chrome DevTools endpoint (default: %(default)s)")
    parser.add_argument("mode", nargs="?", choices=("repl", "acp"), default="repl")
    return parser.parse_args()


def main() -> int:
    try:
        args = parse_args()
        return asyncio.run(ACPServer(args.cdp_url).run()) if args.mode == "acp" else asyncio.run(repl(args.cdp_url))
    except Exception as exc:
        print(f"Failed to connect to Chrome: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
