#!/usr/bin/env python3
"""
migrate_ipv6_egress.py - Migrates CPA's single-account-per-IPv6 (WARP) egress topology into grok2api.

Topology:
  - 16 distinct WARP endpoints on huawei-sg (10.144.144.13:10881 ~ 10896)
  - Each endpoint exits through a dedicated, distinct IPv6 address
  - Sticky assignment: crc32(email) % 16 -> egress_node_id
  - All 2,103 Grok Web accounts bound 100% to their deterministic IPv6 egress node
"""

import sys
import os
import json
import zlib
import sqlite3
import urllib.request
import urllib.parse
from collections import defaultdict

BASE_URL = os.environ.get("GROK2API_BASE_URL", "http://127.0.0.1:8000")
ADMIN_USER = os.environ.get("GROK2API_ADMIN_USER", "admin")
ADMIN_PASS = os.environ.get("GROK2API_ADMIN_PASS", "admin_mango_2026")
DB_PATH = "/Users/mango/project/grok2api/data/backend.db"
HUAWEI_SG_IP = "10.144.144.13"
START_PORT = 10881
NUM_ENDPOINTS = 16


def http_post_json(url, payload, token=None, timeout=30):
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def http_get_json(url, token=None, timeout=30):
    req = urllib.request.Request(url, method="GET")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def login():
    url = f"{BASE_URL}/api/admin/v1/auth/login"
    res = http_post_json(url, {"username": ADMIN_USER, "password": ADMIN_PASS})
    token = res.get("data", {}).get("tokens", {}).get("accessToken")
    if not token:
        raise RuntimeError(f"Login failed: {res}")
    return token


def get_existing_nodes(token):
    url = f"{BASE_URL}/api/admin/v1/egress-nodes?pageSize=100"
    res = http_get_json(url, token=token)
    items = res.get("data", {}).get("items", [])
    node_map = {}
    for item in items:
        node_map[item["name"]] = item
    return node_map


def ensure_nodes(token):
    existing = get_existing_nodes(token)
    nodes_by_idx = {}

    print(f"[*] Ensuring {NUM_ENDPOINTS} WARP IPv6 egress nodes...")
    for i in range(NUM_ENDPOINTS):
        name = f"warp-ipv6-ep{i+1:02d}"
        port = START_PORT + i
        proxy_url = f"socks5://{HUAWEI_SG_IP}:{port}"

        if name in existing:
            node_info = existing[name]
            node_id = str(node_info["id"])
            print(f"  [=] Node {name} already exists (ID: {node_id})")
        else:
            payload = {
                "name": name,
                "scope": "grok_web",
                "accountCapacity": 0,
                "proxyUrl": proxy_url
            }
            res = http_post_json(f"{BASE_URL}/api/admin/v1/egress-nodes", payload, token=token)
            node_id = str(res.get("data", {}).get("id"))
            print(f"  [+] Created node {name} (ID: {node_id}) -> {proxy_url}")

        nodes_by_idx[i] = {
            "name": name,
            "port": port,
            "id": node_id,
            "proxy_url": proxy_url
        }

    return nodes_by_idx


def probe_nodes(token, nodes_by_idx):
    print("\n[*] Probing all 16 egress nodes for dual-stack IPv4/IPv6 health...")
    for idx, info in sorted(nodes_by_idx.items()):
        node_id = info["id"]
        name = info["name"]
        try:
            res = http_post_json(f"{BASE_URL}/api/admin/v1/egress-nodes/{node_id}/test", {}, token=token, timeout=15)
            data = res.get("data", {})
            ipv4 = data.get("ipv4", {}).get("exitIp", "unknown")
            ipv6 = data.get("ipv6", {}).get("exitIp", "unknown")
            latency = data.get("latencyMs", 0)
            status = data.get("status", "unknown")
            info["ipv4"] = ipv4
            info["ipv6"] = ipv6
            info["latencyMs"] = latency
            info["status"] = status
            print(f"  [✓] {name} (Port {info['port']}): {status.upper()} | IPv6={ipv6} | v4={ipv4} | {latency}ms")
        except Exception as e:
            print(f"  [!] {name} (Port {info['port']}): Probe failed: {e}")
            info["status"] = "error"


def assign_accounts_to_nodes(token, nodes_by_idx):
    print("\n[*] Fetching grok_web accounts from SQLite...")
    conn = sqlite3.connect(DB_PATH)
    cursor = conn.cursor()
    cursor.execute("SELECT id, email, name FROM provider_accounts WHERE provider = 'grok_web'")
    rows = cursor.fetchall()
    conn.close()

    print(f"[+] Loaded {len(rows)} grok_web accounts.")

    # Group by crc32(email) % 16
    grouped = defaultdict(list)
    for acc_id, email, name in rows:
        identifier = email or name or str(acc_id)
        bucket = zlib.crc32(identifier.encode("utf-8")) % NUM_ENDPOINTS
        grouped[bucket].append(str(acc_id))

    print("[*] Binding accounts to nodes using deterministic crc32(email) % 16...")
    total_bound = 0
    for idx, account_ids in sorted(grouped.items()):
        node_info = nodes_by_idx[idx]
        node_id = node_info["id"]
        node_name = node_info["name"]

        # Batch assignment (chunk by 200)
        chunk_size = 200
        assigned_for_node = 0
        for c_start in range(0, len(account_ids), chunk_size):
            chunk = account_ids[c_start : c_start + chunk_size]
            payload = {
                "provider": "grok_web",
                "ids": chunk,
                "mode": "manual"
            }
            res = http_post_json(f"{BASE_URL}/api/admin/v1/egress-nodes/{node_id}/accounts", payload, token=token)
            assigned_for_node += res.get("data", {}).get("assigned", len(chunk))

        nodes_by_idx[idx]["account_count"] = assigned_for_node
        total_bound += assigned_for_node
        print(f"  [+] {node_name} (Port {node_info['port']}): Bound {assigned_for_node} accounts.")

    print(f"\n[🎉] Total accounts successfully bound to dedicated IPv6 egress nodes: {total_bound}/{len(rows)}")


def print_summary(nodes_by_idx):
    print("\n" + "=" * 90)
    print("🚀 GROK WEB POOL EGRESS TOPOLOGY SUMMARY")
    print("=" * 90)
    print(f"{'Node Name':<18} | {'Port':<6} | {'Status':<8} | {'IPv6 Exit':<35} | {'Accounts':<8}")
    print("-" * 90)
    for idx, info in sorted(nodes_by_idx.items()):
        name = info["name"]
        port = str(info["port"])
        status = info.get("status", "unknown")
        ipv6 = info.get("ipv6", "unknown")
        count = str(info.get("account_count", 0))
        print(f"{name:<18} | {port:<6} | {status:<8} | {ipv6:<35} | {count:<8}")
    print("=" * 90 + "\n")


def main():
    print("[*] Starting IPv6 Egress migration to grok2api...")
    token = login()
    nodes_by_idx = ensure_nodes(token)
    probe_nodes(token, nodes_by_idx)
    assign_accounts_to_nodes(token, nodes_by_idx)
    print_summary(nodes_by_idx)


if __name__ == "__main__":
    main()
