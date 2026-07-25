#!/usr/bin/env python3
"""SSO cookie → CPA OIDC tokens via xAI device flow (curl_cffi Chrome TLS).

Reads JSON from stdin:
  {"sso":"...","proxy":"http://127.0.0.1:7897","timeout":30,"poll_timeout":90}

Writes JSON to stdout:
  {"ok":true,"access_token":"...","refresh_token":"...","id_token":"...","expires_in":21600}
  or {"ok":false,"error":"..."}
"""
from __future__ import annotations

import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
SCOPE = "openid profile email offline_access grok-cli:access api:access"
DEVICE_CODE_URL = "https://auth.x.ai/oauth2/device/code"
TOKEN_URL = "https://auth.x.ai/oauth2/token"
VERIFY_URL = "https://auth.x.ai/oauth2/device/verify"
APPROVE_URL = "https://auth.x.ai/oauth2/device/approve"


def _fail(msg: str, code: int = 1) -> None:
    sys.stdout.write(json.dumps({"ok": False, "error": msg}, ensure_ascii=False) + "\n")
    sys.exit(code)


def _post_json_form(url: str, form: dict[str, str], timeout: float, proxy: str | None) -> tuple[int, Any]:
    data = urllib.parse.urlencode(form).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Accept": "application/json",
            "User-Agent": "grok-shell/0.2.111 (windows; amd64)",
        },
        method="POST",
    )
    handlers: list[Any] = []
    if proxy:
        handlers.append(urllib.request.ProxyHandler({"http": proxy, "https": proxy}))
    opener = urllib.request.build_opener(*handlers) if handlers else urllib.request.build_opener()
    try:
        with opener.open(req, timeout=timeout) as resp:
            body = resp.read().decode("utf-8", errors="replace")
            status = getattr(resp, "status", 200) or 200
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", errors="replace")
        status = e.code
    except Exception as e:  # noqa: BLE001
        raise RuntimeError(f"http {url}: {e}") from e
    try:
        return status, json.loads(body)
    except Exception:
        return status, body


def main() -> None:
    try:
        raw = sys.stdin.read()
        cfg = json.loads(raw or "{}")
    except Exception as e:  # noqa: BLE001
        _fail(f"invalid stdin json: {e}")

    sso = str(cfg.get("sso") or "").strip()
    if not sso:
        _fail("missing sso")
    proxy = str(cfg.get("proxy") or "").strip() or None
    timeout = float(cfg.get("timeout") or 30)
    poll_timeout = float(cfg.get("poll_timeout") or 90)

    try:
        from curl_cffi import requests as cf_requests
    except ImportError as e:
        _fail(f"curl_cffi not installed: {e}")

    # Fix Windows curl_cffi CA errors (curl 77): non-ASCII user paths break libcurl.
    # Prefer ASCII path scripts/cacert.pem shipped next to this file.
    import os

    ca_file = None
    script_ca = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cacert.pem")
    if os.path.isfile(script_ca):
        ca_file = script_ca
    else:
        try:
            import certifi
            import shutil
            import tempfile

            src = certifi.where()
            if src and os.path.isfile(src):
                # Copy to ASCII-only temp path if certifi path has non-ASCII chars.
                if any(ord(ch) > 127 for ch in src):
                    dst = os.path.join(tempfile.gettempdir(), "grok_switch_cacert.pem")
                    # Prefer pure-ASCII drive temp when possible.
                    if any(ord(ch) > 127 for ch in dst):
                        dst = os.path.join("C:\\", "grok_switch_cacert.pem")
                    try:
                        shutil.copyfile(src, dst)
                        ca_file = dst
                    except Exception:
                        ca_file = None
                else:
                    ca_file = src
        except Exception:
            ca_file = None
    if ca_file:
        os.environ["SSL_CERT_FILE"] = ca_file
        os.environ["REQUESTS_CA_BUNDLE"] = ca_file
        os.environ["CURL_CA_BUNDLE"] = ca_file

    session = cf_requests.Session()
    if proxy:
        session.proxies = {"http": proxy, "https": proxy}
    verify_opt: Any = ca_file if ca_file else True

    for domain in (".x.ai", "accounts.x.ai", "auth.x.ai", ".accounts.x.ai", ".auth.x.ai"):
        try:
            session.cookies.set("sso", sso, domain=domain)
            session.cookies.set("sso-rw", sso, domain=domain)
        except Exception:
            try:
                session.cookies.set("sso", sso, domain=domain, path="/")
                session.cookies.set("sso-rw", sso, domain=domain, path="/")
            except Exception:
                pass

    # Optional extra cookies from live Chrome (cf_clearance helps approve).
    extra = cfg.get("cookies") or {}
    if isinstance(extra, dict):
        for name, value in extra.items():
            name = str(name or "").strip()
            value = str(value or "").strip()
            if not name or not value:
                continue
            for domain in (".x.ai", "accounts.x.ai", "auth.x.ai"):
                try:
                    session.cookies.set(name, value, domain=domain, path="/")
                except Exception:
                    pass

    def _get(url: str):
        try:
            return session.get(
                url,
                impersonate="chrome",
                timeout=timeout,
                allow_redirects=True,
                verify=verify_opt,
            )
        except Exception as e1:  # noqa: BLE001
            try:
                return session.get(
                    url,
                    impersonate="chrome",
                    timeout=timeout,
                    allow_redirects=True,
                    verify=False,
                )
            except Exception as e2:  # noqa: BLE001
                raise RuntimeError(f"{e1}; retry verify=False: {e2}") from e2

    def _post(url: str, data: dict):
        try:
            return session.post(
                url,
                data=data,
                headers={"Content-Type": "application/x-www-form-urlencoded"},
                impersonate="chrome",
                timeout=timeout,
                allow_redirects=True,
                verify=verify_opt,
            )
        except Exception as e1:  # noqa: BLE001
            try:
                return session.post(
                    url,
                    data=data,
                    headers={"Content-Type": "application/x-www-form-urlencoded"},
                    impersonate="chrome",
                    timeout=timeout,
                    allow_redirects=True,
                    verify=False,
                )
            except Exception as e2:  # noqa: BLE001
                raise RuntimeError(f"{e1}; retry verify=False: {e2}") from e2

    # 1) Validate SSO
    try:
        r = _get("https://accounts.x.ai/")
    except Exception as e:  # noqa: BLE001
        _fail(f"accounts.x.ai: {e}")
    final_url = str(getattr(r, "url", "") or "")
    if "sign-in" in final_url or "sign-up" in final_url:
        _fail(f"sso invalid landed={final_url[:160]}")

    # 2) Device code (stdlib; no special TLS needed for this endpoint)
    status, payload = _post_json_form(
        DEVICE_CODE_URL,
        {"client_id": CLIENT_ID, "scope": SCOPE},
        timeout,
        proxy,
    )
    if not isinstance(payload, dict) or not payload.get("device_code") or not payload.get("user_code"):
        _fail(f"device_code failed status={status} body={str(payload)[:200]}")
    device_code = str(payload["device_code"])
    user_code = str(payload["user_code"])
    interval = max(int(payload.get("interval") or 5), 2)
    verify_uri = str(
        payload.get("verification_uri_complete")
        or (
            str(payload.get("verification_uri") or "https://accounts.x.ai/oauth2/device")
            + "?user_code="
            + urllib.parse.quote(user_code)
        )
    )

    # 3) Open verification URI (session / CSRF)
    try:
        _get(verify_uri)
    except Exception as e:  # noqa: BLE001
        _fail(f"verification_uri: {e}")

    # 4) verify
    try:
        r = _post(VERIFY_URL, {"user_code": user_code})
    except Exception as e:  # noqa: BLE001
        _fail(f"device/verify: {e}")
    verify_url = str(getattr(r, "url", "") or "")
    if "consent" not in verify_url.lower() and "consent" not in str(getattr(r, "text", "") or "").lower()[:300]:
        # soft continue — some builds still authorize after approve
        pass

    # 5) approve
    try:
        r = _post(
            APPROVE_URL,
            {
                "user_code": user_code,
                "action": "allow",
                "principal_type": "User",
                "principal_id": "",
            },
        )
    except Exception as e:  # noqa: BLE001
        _fail(f"device/approve: {e}")

    # 6) poll token
    deadline = time.time() + min(poll_timeout, float(payload.get("expires_in") or 1800))
    last = "pending"
    while time.time() < deadline:
        status, tok = _post_json_form(
            TOKEN_URL,
            {
                "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                "device_code": device_code,
                "client_id": CLIENT_ID,
            },
            timeout,
            proxy,
        )
        if isinstance(tok, dict) and tok.get("access_token") and tok.get("refresh_token"):
            sys.stdout.write(
                json.dumps(
                    {
                        "ok": True,
                        "access_token": tok["access_token"],
                        "refresh_token": tok["refresh_token"],
                        "id_token": tok.get("id_token") or "",
                        "expires_in": int(tok.get("expires_in") or 0),
                        "user_code": user_code,
                        "mint_method": "curl_cffi",
                    },
                    ensure_ascii=False,
                )
                + "\n"
            )
            return
        if isinstance(tok, dict):
            err = str(tok.get("error") or "")
            desc = str(tok.get("error_description") or "")
            last = f"{err} {desc}".strip() or f"http_{status}"
            if err in ("access_denied", "expired_token") or (
                err == "invalid_grant" and "access denied" in desc.lower()
            ):
                _fail(f"token denied: {last}")
            if err == "slow_down":
                time.sleep(interval + 3)
                continue
            if err == "authorization_pending":
                time.sleep(interval)
                continue
            _fail(f"token error: {last}")
        last = f"http_{status} {str(tok)[:120]}"
        time.sleep(interval)
    _fail(f"token poll timeout last={last}")


if __name__ == "__main__":
    main()
