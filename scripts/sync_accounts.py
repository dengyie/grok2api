#!/usr/bin/env python3
"""
sync_accounts.py - Batch synchronizes accounts_cli.txt into grok2api web account pool.
"""

import sys
import os
import json
import urllib.request
import urllib.parse
import uuid

ACCOUNTS_FILE = "/Users/mango/project/claude-project/grok-register/accounts_cli.txt"
BASE_URL = os.environ.get("GROK2API_BASE_URL", "http://127.0.0.1:8000")
ADMIN_USER = os.environ.get("GROK2API_ADMIN_USER", "admin")
ADMIN_PASS = os.environ.get("GROK2API_ADMIN_PASS", "admin_mango_2026")


def http_post_json(url, payload, token=None):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8"))


def http_get_json(url, token=None):
    req = urllib.request.Request(url, method="GET")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode("utf-8"))


def login():
    url = f"{BASE_URL}/api/admin/v1/auth/login"
    res = http_post_json(url, {"username": ADMIN_USER, "password": ADMIN_PASS})
    token = res.get("data", {}).get("tokens", {}).get("accessToken")
    if not token:
        raise RuntimeError(f"Login failed: {res}")
    return token


def ensure_client_key(token):
    # Check if a client key already exists
    url = f"{BASE_URL}/api/admin/v1/client-keys"
    res = http_get_json(url, token=token)
    items = res.get("data", {}).get("items", [])
    if items:
        key_id = items[0].get("id")
        key_name = items[0].get("name")
        print(f"[*] Found existing client key: {key_name} (ID: {key_id})")
        # Reveal secret
        try:
            sec_res = http_get_json(f"{url}/{key_id}/secret", token=token)
            secret = sec_res.get("data", {}).get("secret")
            if secret:
                return secret
        except Exception:
            pass

    # Create new client key
    print("[+] Creating new default client key...")
    create_res = http_post_json(url, {"name": "cpa-grok-gateway", "rpmLimit": 0, "maxConcurrent": 0}, token=token)
    secret = create_res.get("data", {}).get("secret")
    print(f"[+] Client Key created: {secret}")
    return secret


def read_accounts():
    if not os.path.exists(ACCOUNTS_FILE):
        raise FileNotFoundError(f"Accounts file not found: {ACCOUNTS_FILE}")

    accounts = []
    seen = set()
    with open(ACCOUNTS_FILE, "r", encoding="utf-8") as f:
        for line_num, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            parts = line.split("----")
            if len(parts) >= 3:
                email = parts[0].strip()
                sso_token = parts[2].strip()
                if sso_token.startswith("sso="):
                    sso_token = sso_token[4:]
                if not sso_token:
                    continue
                if sso_token in seen:
                    continue
                seen.add(sso_token)
                accounts.append({
                    "name": email,
                    "email": email,
                    "sso_token": sso_token,
                    "tier": "basic"
                })
    return accounts


def upload_accounts_chunk(token, chunk, batch_idx, total_batches):
    boundary = f"----WebKitFormBoundary{uuid.uuid4().hex}"
    url = f"{BASE_URL}/api/admin/v1/accounts/web/import"

    file_bytes = json.dumps(chunk, ensure_ascii=False).encode("utf-8")
    
    body = bytearray()
    body.extend(f"--{boundary}\r\n".encode("utf-8"))
    body.extend(b'Content-Disposition: form-data; name="file"; filename="accounts.json"\r\n')
    body.extend(b'Content-Type: application/json\r\n\r\n')
    body.extend(file_bytes)
    body.extend(f"\r\n--{boundary}--\r\n".encode("utf-8"))

    req = urllib.request.Request(url, data=bytes(body), method="POST")
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Content-Type", f"multipart/form-data; boundary={boundary}")

    print(f"[+] Uploading batch {batch_idx}/{total_batches} ({len(chunk)} accounts)...")
    with urllib.request.urlopen(req, timeout=300) as resp:
        # Response is an SSE stream
        raw_text = resp.read().decode("utf-8")
        created = 0
        updated = 0
        skipped = 0
        failed = 0
        for block in raw_text.split("\n\n"):
            lines = [ln.strip() for ln in block.split("\n") if ln.strip()]
            event_name = ""
            event_data = ""
            for ln in lines:
                if ln.startswith("event:"):
                    event_name = ln[6:].strip()
                elif ln.startswith("data:"):
                    event_data = ln[5:].strip()
            if event_name == "complete" and event_data:
                try:
                    payload = json.loads(event_data)
                    created = payload.get("created", 0)
                    updated = payload.get("updated", 0)
                    skipped = payload.get("skipped", 0)
                    failed = payload.get("failed", 0)
                except Exception:
                    pass
        print(f"    -> Batch {batch_idx} completed: created={created}, updated={updated}, skipped={skipped}, failed={failed}")
        return created, updated, skipped, failed


def main():
    print(f"[*] Reading accounts from {ACCOUNTS_FILE}...")
    accounts = read_accounts()
    print(f"[+] Loaded {len(accounts)} unique valid SSO accounts.")

    print(f"[*] Logging in to grok2api ({BASE_URL})...")
    token = login()
    print("[+] Admin login successful.")

    client_key = ensure_client_key(token)

    # Chunking by 500
    chunk_size = 500
    total_batches = (len(accounts) + chunk_size - 1) // chunk_size

    total_created = 0
    total_updated = 0
    total_skipped = 0
    total_failed = 0

    for i in range(total_batches):
        chunk = accounts[i * chunk_size : (i + 1) * chunk_size]
        created, updated, skipped, failed = upload_accounts_chunk(token, chunk, i + 1, total_batches)
        total_created += created
        total_updated += updated
        total_skipped += skipped
        total_failed += failed

    print("\n" + "=" * 50)
    print("🎉 Grok Accounts Sync Summary:")
    print(f"  - Total Accounts Read: {len(accounts)}")
    print(f"  - Newly Created:      {total_created}")
    print(f"  - Updated:            {total_updated}")
    print(f"  - Skipped (Duplicate):{total_skipped}")
    print(f"  - Failed:             {total_failed}")
    print(f"  - Gateway API Key:    {client_key}")
    print(f"  - Base URL:           {BASE_URL}/v1")
    print("=" * 50 + "\n")


if __name__ == "__main__":
    main()
