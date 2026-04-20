#!/usr/bin/env python3
"""
Complete login example — same flow as the vendor app.

Steps:
1. Password login attempt → triggers MFA_NEED_SEND_CODE
2. Request MFA code → sent to user's email
3. User enters 6-digit code
4. Login with password + MFA code → SID

Environment variables:
    TUYA_SIGNING_KEY  — HMAC key (extract from APK)
    TUYA_APP_KEY      — Tuya app key
    TUYA_EMAIL        — User email
    TUYA_PASSWORD     — User password
    TUYA_COUNTRY      — Country code (default: 39)
"""

import os
import sys

sys.path.insert(0, os.path.dirname(__file__))
from tuya_client import TuyaClient, TuyaAPIError

SIGNING_KEY = os.environ["TUYA_SIGNING_KEY"]
APP_KEY = os.environ["TUYA_APP_KEY"]
EMAIL = os.environ["TUYA_EMAIL"]
PASSWORD = os.environ["TUYA_PASSWORD"]
COUNTRY = os.environ.get("TUYA_COUNTRY", "39")

client = TuyaClient(signing_key=SIGNING_KEY, app_key=APP_KEY)

# Step 1: Attempt login (triggers MFA)
print(f"Logging in as {EMAIL}...")
try:
    client.login(EMAIL, PASSWORD, COUNTRY, mfa_code="")
    print("Login succeeded without MFA (unexpected)")
except TuyaAPIError as e:
    if e.code != "MFA_NEED_SEND_CODE":
        print(f"Unexpected error: {e}")
        sys.exit(1)
    print("MFA required (expected)")

# Step 2: Request MFA code
print("Requesting MFA code...")
result = client.trigger_mfa(EMAIL, PASSWORD, COUNTRY)
print(f"MFA code sent to: {result.get('email', EMAIL)}")

# Step 3: Get code from user
mfa_code = input("Enter 6-digit MFA code from email: ").strip()

# Step 4: Login with MFA
sid = client.login(EMAIL, PASSWORD, COUNTRY, mfa_code=mfa_code)
print("\nLogin successful!")
print(f"SID: {sid}")

# Verify
user = client.get_user_info()
print(f"User: {user['nickname']} ({user['email']})")
print(f"MQTT: {user['domain']['mobileMqttsUrl']}")

# Save SID for later use
print("\nExport for other scripts:")
print(f'export TUYA_SID="{sid}"')
