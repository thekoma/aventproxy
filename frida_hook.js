// ============================================================================
// Frida Hook Script - Philips Avent SCD973 Baby Monitor AES Key Extraction
// Target: Philips Baby Monitor+ Android App (com.philips.ph.babymonitorplus)
// Purpose: Intercept AES-128 encryption keys used for video stream encryption
// ============================================================================
//
// SETUP INSTRUCTIONS
// ==================
//
// 1. PREREQUISITES
//    - Rooted Android device or emulator (Magisk recommended)
//    - Frida server matching your device arch (arm64 for most modern devices)
//    - Frida tools on your PC: pip install frida-tools
//    - USB debugging enabled on the device
//
// 2. INSTALL FRIDA SERVER ON DEVICE
//    Download from: https://github.com/frida/frida/releases
//    Pick the right arch (e.g., frida-server-16.x.x-android-arm64)
//
//    adb push frida-server-16.x.x-android-arm64 /data/local/tmp/frida-server
//    adb shell "chmod 755 /data/local/tmp/frida-server"
//    adb shell "su -c /data/local/tmp/frida-server &"
//
// 3. RUN THIS SCRIPT
//    Option A - Attach to running app:
//      frida -U -l frida_hook.js com.philips.ph.babymonitorplus
//
//    Option B - Spawn the app (recommended, catches early init):
//      frida -U -f com.philips.ph.babymonitorplus -l frida_hook.js --no-pause
//
//    Option C - If using frida over network (device IP e.g. 192.168.1.50):
//      frida -H 192.168.1.50:27042 -f com.philips.ph.babymonitorplus -l frida_hook.js --no-pause
//
// 4. WHAT TO LOOK FOR IN OUTPUT
//    - Tags starting with [AES-KEY] contain the actual encryption keys
//    - [JAVA:setEncryptionInfo] shows the JSON with device ID and secret keys
//    - [NATIVE:SetEncryptKey] shows the raw key string passed to native code
//    - [NATIVE:GetEncryptKey] shows what the native layer returns as the key
//    - [NATIVE:aes_decrypt_with_raw_key] shows the key used for decryption
//    - [NATIVE:sdp_set_aes_key] / [NATIVE:sdp_get_aes_key] show P2P SDP keys
//    - [JAVA:Hmac.a] shows the HMAC-SHA256 key derivation inputs
//    - [JAVA:AES.a] shows the encryption method params (str2=HMAC data, str3=HMAC key)
//    - [OPENSSL:AES_set_*_key] shows OpenSSL-level key material
//    - [OPENSSL:EVP_DecryptInit_ex] shows cipher mode and key for decryption
//
//    The AES-128 key is 16 bytes. Look for hex dumps of exactly 16 bytes.
//    The key flow is typically:
//      secretKey (from cloud API) -> Hmac.a(secretKey.bytes, localKey) -> first 16 bytes = AES key
//      -> setEncryptionInfo(JSON) -> native SetEncryptKey -> OpenSSL AES_set_decrypt_key
//
// 5. TIPS
//    - Start a live preview in the app to trigger key exchange
//    - Playback recordings also triggers setPlaybackEncryption with per-fragment keys
//    - If you see "module not loaded yet", wait for the app to fully start
//    - Use -f (spawn) mode to catch keys set during initialization
//
// ============================================================================

"use strict";

// ---------------------------------------------------------------------------
// Utility helpers
// ---------------------------------------------------------------------------

function hexdump_bytes(arr, max_len) {
    max_len = max_len || 64;
    var len = Math.min(arr.length, max_len);
    var hex = [];
    var ascii = [];
    var lines = [];

    for (var i = 0; i < len; i++) {
        var b = (typeof arr[i] === "number") ? arr[i] & 0xff : arr[i];
        hex.push(("0" + b.toString(16)).slice(-2));
        ascii.push((b >= 0x20 && b <= 0x7e) ? String.fromCharCode(b) : ".");

        if ((i + 1) % 16 === 0 || i === len - 1) {
            var offset_str = ("0000" + (i - (i % 16)).toString(16)).slice(-4);
            var hex_str = hex.join(" ");
            // pad hex string to fixed width
            while (hex_str.length < 48) hex_str += " ";
            lines.push(offset_str + "  " + hex_str + "  |" + ascii.join("") + "|");
            hex = [];
            ascii = [];
        }
    }
    if (arr.length > max_len) {
        lines.push("... (" + arr.length + " bytes total, showing first " + max_len + ")");
    }
    return lines.join("\n");
}

function ptr_to_hex(ptr, len) {
    if (ptr.isNull()) return "(null)";
    try {
        var buf = ptr.readByteArray(len);
        if (!buf) return "(unreadable)";
        var arr = new Uint8Array(buf);
        var parts = [];
        for (var i = 0; i < arr.length; i++) {
            parts.push(("0" + arr[i].toString(16)).slice(-2));
        }
        return parts.join("");
    } catch (e) {
        return "(read error: " + e.message + ")";
    }
}

function read_cstring(ptr, max_len) {
    if (ptr.isNull()) return "(null)";
    max_len = max_len || 4096;
    try {
        return ptr.readUtf8String(max_len);
    } catch (e) {
        return "(read error: " + e.message + ")";
    }
}

function safe_backtrace() {
    try {
        return Thread.backtrace(this.context, Backtracer.ACCURATE)
            .map(DebugSymbol.fromAddress).join("\n    ");
    } catch (e) {
        return "(backtrace unavailable)";
    }
}

function log_key_banner(tag, key_hex) {
    console.log("\n" +
        "##########################################################\n" +
        "# [AES-KEY] " + tag + "\n" +
        "# KEY (hex): " + key_hex + "\n" +
        "##########################################################\n");
}

// ---------------------------------------------------------------------------
// SECTION 1: Java-layer hooks
// ---------------------------------------------------------------------------

function hook_java() {
    Java.perform(function () {
        console.log("[*] Java.perform: hooking Java methods...");

        // --- 1. ThingCameraNative.setEncryptionInfo(long, String) ---
        try {
            var ThingCameraNative = Java.use("com.thingclips.smart.camera.nativeapi.ThingCameraNative");
            ThingCameraNative.setEncryptionInfo.implementation = function (handle, jsonStr) {
                console.log("\n[JAVA:setEncryptionInfo] ========================================");
                console.log("[JAVA:setEncryptionInfo] handle = " + handle);
                console.log("[JAVA:setEncryptionInfo] JSON   = " + jsonStr);
                try {
                    var parsed = JSON.parse(jsonStr);
                    if (parsed.encryptInfos) {
                        for (var i = 0; i < parsed.encryptInfos.length; i++) {
                            var info = parsed.encryptInfos[i];
                            log_key_banner("setEncryptionInfo[" + i + "] uuid=" + (info.uuid || "?"), info.encrypt || "(none)");
                        }
                    }
                    if (parsed.encrypt) {
                        log_key_banner("setEncryptionInfo (single)", parsed.encrypt);
                    }
                } catch (e) {
                    console.log("[JAVA:setEncryptionInfo] (JSON parse failed: " + e.message + ")");
                }
                console.log("[JAVA:setEncryptionInfo] ========================================");
                return this.setEncryptionInfo(handle, jsonStr);
            };
            console.log("[+] Hooked ThingCameraNative.setEncryptionInfo");
        } catch (e) {
            console.log("[-] Failed to hook ThingCameraNative.setEncryptionInfo: " + e.message);
        }

        // --- 7. AES.a(String, String, String) - encryption with key derivation ---
        try {
            var AES = Java.use("com.thingclips.smart.camera.utils.AES");
            AES.a.overload("java.lang.String", "java.lang.String", "java.lang.String").implementation = function (str, str2, str3) {
                console.log("\n[JAVA:AES.a] =============================================");
                console.log("[JAVA:AES.a] str  (plaintext) = " + str);
                console.log("[JAVA:AES.a] str2 (HMAC data) = " + str2);
                console.log("[JAVA:AES.a] str3 (HMAC key)  = " + str3);
                console.log("[JAVA:AES.a] Key derivation: HMAC-SHA256(str2.getBytes(), str3) -> first 16 bytes -> AES key");
                console.log("[JAVA:AES.a] =============================================");
                var result = this.a(str, str2, str3);
                console.log("[JAVA:AES.a] result (base64 ciphertext) = " + result);
                return result;
            };
            console.log("[+] Hooked AES.a(String, String, String)");
        } catch (e) {
            console.log("[-] Failed to hook AES.a: " + e.message);
        }

        // --- 8. Hmac.a(byte[], String) - HMAC-SHA256 ---
        try {
            var Hmac = Java.use("com.thingclips.smart.camera.utils.Hmac");
            Hmac.a.overload("[B", "java.lang.String").implementation = function (data, key) {
                console.log("\n[JAVA:Hmac.a] ============================================");
                console.log("[JAVA:Hmac.a] key (String)  = " + key);
                var dataBytes = [];
                for (var i = 0; i < data.length; i++) dataBytes.push(data[i] & 0xff);
                console.log("[JAVA:Hmac.a] data (bytes, len=" + data.length + "):");
                console.log(hexdump_bytes(dataBytes, 128));
                try {
                    console.log("[JAVA:Hmac.a] data (as UTF-8) = " + Java.use("java.lang.String").$new(data, "UTF-8"));
                } catch (e) { /* not valid UTF-8 */ }

                var result = this.a(data, key);

                var resultBytes = [];
                for (var j = 0; j < result.length; j++) resultBytes.push(result[j] & 0xff);
                console.log("[JAVA:Hmac.a] HMAC-SHA256 result (" + result.length + " bytes):");
                console.log(hexdump_bytes(resultBytes, 32));

                // The first 16 bytes of this HMAC output become the AES-128 key
                var keyHex = [];
                for (var k = 0; k < Math.min(16, resultBytes.length); k++) {
                    keyHex.push(("0" + resultBytes[k].toString(16)).slice(-2));
                }
                log_key_banner("HMAC-SHA256 -> AES-128 (first 16 bytes)", keyHex.join(""));
                console.log("[JAVA:Hmac.a] ============================================");
                return result;
            };
            console.log("[+] Hooked Hmac.a(byte[], String)");
        } catch (e) {
            console.log("[-] Failed to hook Hmac.a: " + e.message);
        }

        // --- Hook the wrapper: pdqppqb.setEncryptionInfo (ThingCameraImpl) ---
        try {
            var ThingCameraImpl = Java.use("com.thingclips.smart.camera.v2.pdqppqb");
            ThingCameraImpl.setEncryptionInfo.overload("java.lang.String").implementation = function (str) {
                console.log("\n[JAVA:ThingCameraImpl.setEncryptionInfo] ==================");
                console.log("[JAVA:ThingCameraImpl.setEncryptionInfo] encrypt = " + str);
                console.log("[JAVA:ThingCameraImpl.setEncryptionInfo] ==================");
                return this.setEncryptionInfo(str);
            };
            console.log("[+] Hooked ThingCameraImpl(pdqppqb).setEncryptionInfo");
        } catch (e) {
            console.log("[-] Failed to hook pdqppqb.setEncryptionInfo: " + e.message);
        }

        console.log("[*] Java hooks installed.");
    });
}

// ---------------------------------------------------------------------------
// SECTION 2: Native-layer hooks (libThingCameraSDK.so)
// ---------------------------------------------------------------------------

function hook_native_camera_sdk() {
    var mod_name = "libThingCameraSDK.so";
    var mod = Process.findModuleByName(mod_name);
    if (!mod) {
        console.log("[-] " + mod_name + " not loaded yet, will retry on module load...");
        return false;
    }
    console.log("[*] Found " + mod_name + " at " + mod.base + " (size " + mod.size + ")");

    // --- 2. SetEncryptionInfo (ThingCameraBase::SetEncryptionInfo) ---
    var sym_set_enc_info = Module.findExportByName(mod_name, "_ZN15ThingCameraBase17SetEncryptionInfoEPKc");
    if (sym_set_enc_info) {
        Interceptor.attach(sym_set_enc_info, {
            onEnter: function (args) {
                // args[0] = this, args[1] = const char* json
                var json_str = read_cstring(args[1]);
                console.log("\n[NATIVE:SetEncryptionInfo] ====================================");
                console.log("[NATIVE:SetEncryptionInfo] json = " + json_str);
                console.log("[NATIVE:SetEncryptionInfo] ====================================");
            }
        });
        console.log("[+] Hooked ThingCameraBase::SetEncryptionInfo @ " + sym_set_enc_info);
    } else {
        console.log("[-] ThingCameraBase::SetEncryptionInfo not found");
    }

    // --- Also hook the JNI entry point ---
    var sym_jni_set_enc = Module.findExportByName(mod_name,
        "Java_com_thingclips_smart_camera_nativeapi_ThingCameraNative_setEncryptionInfo");
    if (sym_jni_set_enc) {
        Interceptor.attach(sym_jni_set_enc, {
            onEnter: function (args) {
                // args[0] = JNIEnv*, args[1] = jclass, args[2] = jlong handle, args[3] = jstring
                var env = args[0];
                var jstr = args[3];
                if (!jstr.isNull()) {
                    var JNIEnv = Java.vm.getEnv();
                    var str = JNIEnv.getStringUtfChars(jstr, null).readUtf8String();
                    console.log("\n[NATIVE:JNI_setEncryptionInfo] ================================");
                    console.log("[NATIVE:JNI_setEncryptionInfo] handle = " + args[2].toInt32());
                    console.log("[NATIVE:JNI_setEncryptionInfo] json   = " + str);
                    console.log("[NATIVE:JNI_setEncryptionInfo] ================================");
                }
            }
        });
        console.log("[+] Hooked JNI setEncryptionInfo @ " + sym_jni_set_enc);
    }

    // --- 3. SetEncryptKey (ThingPlayTask and ThingDownloadTask) ---
    var set_encrypt_key_symbols = [
        { mangled: "_ZN13ThingPlayTask13SetEncryptKeyEPKc", name: "ThingPlayTask::SetEncryptKey" },
        { mangled: "_ZN13ThingSmartIPC17ThingDownloadTask13SetEncryptKeyEPKc", name: "ThingDownloadTask::SetEncryptKey" }
    ];

    set_encrypt_key_symbols.forEach(function (sym_info) {
        var addr = Module.findExportByName(mod_name, sym_info.mangled);
        if (addr) {
            Interceptor.attach(addr, {
                onEnter: function (args) {
                    // args[0] = this, args[1] = const char* key
                    var key_str = read_cstring(args[1]);
                    console.log("\n[NATIVE:SetEncryptKey] ========================================");
                    console.log("[NATIVE:SetEncryptKey] class = " + sym_info.name);
                    console.log("[NATIVE:SetEncryptKey] key   = " + key_str);
                    console.log("[NATIVE:SetEncryptKey] key (hex):");
                    if (key_str && key_str.length > 0) {
                        var key_bytes = [];
                        for (var i = 0; i < key_str.length; i++) {
                            key_bytes.push(key_str.charCodeAt(i));
                        }
                        console.log(hexdump_bytes(key_bytes));
                        log_key_banner(sym_info.name, ptr_to_hex(args[1], Math.min(key_str.length, 32)));
                    }
                    console.log("[NATIVE:SetEncryptKey] ========================================");
                }
            });
            console.log("[+] Hooked " + sym_info.name + " @ " + addr);
        } else {
            console.log("[-] " + sym_info.name + " not found");
        }
    });

    // --- 4. GetEncryptKey (ThingCameraBase::GetEncryptKey) ---
    var sym_get_key = Module.findExportByName(mod_name, "_ZN15ThingCameraBase13GetEncryptKeyEPKci");
    if (sym_get_key) {
        Interceptor.attach(sym_get_key, {
            onEnter: function (args) {
                // args[0] = this, args[1] = const char* (buffer or identifier), args[2] = int (index or len)
                this.key_ptr = args[1];
                this.key_len = args[2].toInt32();
                console.log("\n[NATIVE:GetEncryptKey] onEnter ================================");
                console.log("[NATIVE:GetEncryptKey] arg1 (str) = " + read_cstring(args[1]));
                console.log("[NATIVE:GetEncryptKey] arg2 (int) = " + this.key_len);
            },
            onLeave: function (retval) {
                console.log("[NATIVE:GetEncryptKey] onLeave ================================");
                console.log("[NATIVE:GetEncryptKey] retval = " + retval);
                // The key might be written into arg1 buffer or returned
                if (!this.key_ptr.isNull()) {
                    var key_str = read_cstring(this.key_ptr);
                    console.log("[NATIVE:GetEncryptKey] buffer content = " + key_str);
                    if (key_str && key_str.length > 0 && key_str.length <= 64) {
                        log_key_banner("GetEncryptKey", ptr_to_hex(this.key_ptr, Math.min(key_str.length, 32)));
                    }
                }
                console.log("[NATIVE:GetEncryptKey] ========================================");
            }
        });
        console.log("[+] Hooked ThingCameraBase::GetEncryptKey @ " + sym_get_key);
    } else {
        console.log("[-] ThingCameraBase::GetEncryptKey not found");
    }

    // --- 9. OpenSSL AES_set_encrypt_key / AES_set_decrypt_key ---
    // int AES_set_encrypt_key(const unsigned char *userKey, const int bits, AES_KEY *key);
    var aes_set_enc = Module.findExportByName(mod_name, "AES_set_encrypt_key");
    if (aes_set_enc) {
        Interceptor.attach(aes_set_enc, {
            onEnter: function (args) {
                var bits = args[1].toInt32();
                var key_len = bits / 8;
                console.log("\n[OPENSSL:AES_set_encrypt_key] =================================");
                console.log("[OPENSSL:AES_set_encrypt_key] bits = " + bits);
                console.log("[OPENSSL:AES_set_encrypt_key] key (" + key_len + " bytes):");
                var key_hex = ptr_to_hex(args[0], key_len);
                console.log(hexdump_bytes(new Uint8Array(args[0].readByteArray(key_len))));
                log_key_banner("AES_set_encrypt_key (" + bits + "-bit)", key_hex);
                console.log("[OPENSSL:AES_set_encrypt_key] =================================");
            }
        });
        console.log("[+] Hooked AES_set_encrypt_key @ " + aes_set_enc);
    }

    var aes_set_dec = Module.findExportByName(mod_name, "AES_set_decrypt_key");
    if (aes_set_dec) {
        Interceptor.attach(aes_set_dec, {
            onEnter: function (args) {
                var bits = args[1].toInt32();
                var key_len = bits / 8;
                console.log("\n[OPENSSL:AES_set_decrypt_key] =================================");
                console.log("[OPENSSL:AES_set_decrypt_key] bits = " + bits);
                console.log("[OPENSSL:AES_set_decrypt_key] key (" + key_len + " bytes):");
                var key_hex = ptr_to_hex(args[0], key_len);
                console.log(hexdump_bytes(new Uint8Array(args[0].readByteArray(key_len))));
                log_key_banner("AES_set_decrypt_key (" + bits + "-bit)", key_hex);
                console.log("[OPENSSL:AES_set_decrypt_key] =================================");
            }
        });
        console.log("[+] Hooked AES_set_decrypt_key @ " + aes_set_dec);
    }

    // --- 10. EVP_DecryptInit_ex ---
    // int EVP_DecryptInit_ex(EVP_CIPHER_CTX *ctx, const EVP_CIPHER *type,
    //                        ENGINE *impl, const unsigned char *key, const unsigned char *iv);
    var evp_decrypt_init = Module.findExportByName(mod_name, "EVP_DecryptInit_ex");
    if (evp_decrypt_init) {
        Interceptor.attach(evp_decrypt_init, {
            onEnter: function (args) {
                var ctx = args[0];
                var cipher_type = args[1];
                var key_ptr = args[3];
                var iv_ptr = args[4];

                console.log("\n[OPENSSL:EVP_DecryptInit_ex] ==================================");
                console.log("[OPENSSL:EVP_DecryptInit_ex] ctx    = " + ctx);
                console.log("[OPENSSL:EVP_DecryptInit_ex] cipher = " + cipher_type);

                // Try to identify the cipher via EVP_CIPHER_nid
                if (!cipher_type.isNull()) {
                    try {
                        var EVP_CIPHER_nid = Module.findExportByName(mod_name, "EVP_CIPHER_nid");
                        var EVP_CIPHER_key_length = Module.findExportByName(mod_name, "EVP_CIPHER_key_length");
                        var EVP_CIPHER_iv_length = Module.findExportByName(mod_name, "EVP_CIPHER_iv_length");
                        var EVP_CIPHER_block_size_fn = Module.findExportByName(mod_name, "EVP_CIPHER_block_size");
                        var OBJ_nid2sn = Module.findExportByName(mod_name, "OBJ_nid2sn");

                        if (EVP_CIPHER_nid && OBJ_nid2sn) {
                            var nid_fn = new NativeFunction(EVP_CIPHER_nid, "int", ["pointer"]);
                            var nid2sn_fn = new NativeFunction(OBJ_nid2sn, "pointer", ["int"]);
                            var nid = nid_fn(cipher_type);
                            var sn_ptr = nid2sn_fn(nid);
                            if (!sn_ptr.isNull()) {
                                console.log("[OPENSSL:EVP_DecryptInit_ex] cipher name = " + sn_ptr.readUtf8String());
                            }
                            console.log("[OPENSSL:EVP_DecryptInit_ex] cipher NID  = " + nid);
                        }
                        if (EVP_CIPHER_key_length) {
                            var kl_fn = new NativeFunction(EVP_CIPHER_key_length, "int", ["pointer"]);
                            console.log("[OPENSSL:EVP_DecryptInit_ex] key_length  = " + kl_fn(cipher_type));
                        }
                        if (EVP_CIPHER_iv_length) {
                            var ivl_fn = new NativeFunction(EVP_CIPHER_iv_length, "int", ["pointer"]);
                            console.log("[OPENSSL:EVP_DecryptInit_ex] iv_length   = " + ivl_fn(cipher_type));
                        }
                        if (EVP_CIPHER_block_size_fn) {
                            var bs_fn = new NativeFunction(EVP_CIPHER_block_size_fn, "int", ["pointer"]);
                            console.log("[OPENSSL:EVP_DecryptInit_ex] block_size  = " + bs_fn(cipher_type));
                        }
                    } catch (e) {
                        console.log("[OPENSSL:EVP_DecryptInit_ex] (cipher info lookup error: " + e.message + ")");
                    }
                }

                if (!key_ptr.isNull()) {
                    // Assume 16 bytes for AES-128, read up to 32 to cover AES-256
                    console.log("[OPENSSL:EVP_DecryptInit_ex] key (first 32 bytes):");
                    try {
                        var key_buf = new Uint8Array(key_ptr.readByteArray(32));
                        console.log(hexdump_bytes(key_buf, 32));
                        log_key_banner("EVP_DecryptInit_ex", ptr_to_hex(key_ptr, 16));
                    } catch (e) {
                        console.log("  (key read error: " + e.message + ")");
                    }
                } else {
                    console.log("[OPENSSL:EVP_DecryptInit_ex] key = NULL (reusing previous key)");
                }

                if (!iv_ptr.isNull()) {
                    console.log("[OPENSSL:EVP_DecryptInit_ex] IV (16 bytes):");
                    try {
                        console.log(hexdump_bytes(new Uint8Array(iv_ptr.readByteArray(16)), 16));
                    } catch (e) {
                        console.log("  (IV read error: " + e.message + ")");
                    }
                } else {
                    console.log("[OPENSSL:EVP_DecryptInit_ex] IV = NULL");
                }
                console.log("[OPENSSL:EVP_DecryptInit_ex] ==================================");
            }
        });
        console.log("[+] Hooked EVP_DecryptInit_ex @ " + evp_decrypt_init);
    }

    // --- Also hook EVP_EncryptInit_ex for completeness ---
    var evp_encrypt_init = Module.findExportByName(mod_name, "EVP_EncryptInit_ex");
    if (evp_encrypt_init) {
        Interceptor.attach(evp_encrypt_init, {
            onEnter: function (args) {
                var key_ptr = args[3];
                var iv_ptr = args[4];
                if (!key_ptr.isNull()) {
                    console.log("\n[OPENSSL:EVP_EncryptInit_ex] key (16 bytes): " + ptr_to_hex(key_ptr, 16));
                    if (!iv_ptr.isNull()) {
                        console.log("[OPENSSL:EVP_EncryptInit_ex] IV  (16 bytes): " + ptr_to_hex(iv_ptr, 16));
                    }
                }
            }
        });
        console.log("[+] Hooked EVP_EncryptInit_ex @ " + evp_encrypt_init);
    }

    // --- Scan for any other encrypt-key-related exports via enumeration ---
    console.log("[*] Scanning " + mod_name + " exports for additional encrypt/key symbols...");
    var extra_count = 0;
    mod.enumerateExports().forEach(function (exp) {
        var name_lower = exp.name.toLowerCase();
        if ((name_lower.indexOf("encryptkey") !== -1 || name_lower.indexOf("encrypt_key") !== -1) &&
            // Skip symbols we already hooked
            exp.name.indexOf("AES_set_") === -1 &&
            exp.name.indexOf("ThingPlayTask") === -1 &&
            exp.name.indexOf("ThingDownloadTask") === -1 &&
            exp.name.indexOf("ThingCameraBase") === -1) {
            console.log("[*] Extra export found: " + exp.name + " @ " + exp.address);
            extra_count++;
            try {
                Interceptor.attach(exp.address, {
                    onEnter: function (args) {
                        console.log("\n[NATIVE:EXTRA:" + exp.name + "] called");
                        console.log("[NATIVE:EXTRA:" + exp.name + "] arg0 = " + args[0]);
                        console.log("[NATIVE:EXTRA:" + exp.name + "] arg1 = " + args[1]);
                        try {
                            console.log("[NATIVE:EXTRA:" + exp.name + "] arg1 as string = " + read_cstring(args[1]));
                        } catch (e) {}
                    }
                });
            } catch (e) {
                console.log("[-] Failed to hook extra symbol " + exp.name + ": " + e.message);
            }
        }
    });
    if (extra_count === 0) {
        console.log("[*] No additional encrypt-key symbols found in " + mod_name);
    }

    return true;
}

// ---------------------------------------------------------------------------
// SECTION 3: Native-layer hooks (libThingP2PSDK.so)
// ---------------------------------------------------------------------------

function hook_native_p2p_sdk() {
    var mod_name = "libThingP2PSDK.so";
    var mod = Process.findModuleByName(mod_name);
    if (!mod) {
        console.log("[-] " + mod_name + " not loaded yet, will retry on module load...");
        return false;
    }
    console.log("[*] Found " + mod_name + " at " + mod.base + " (size " + mod.size + ")");

    // --- 5. aes_decrypt_with_raw_key ---
    // Likely signature: int aes_decrypt_with_raw_key(const uint8_t *key, int key_len,
    //                       const uint8_t *in, int in_len, uint8_t *out, int *out_len)
    var sym_aes_decrypt = Module.findExportByName(mod_name, "aes_decrypt_with_raw_key");
    if (sym_aes_decrypt) {
        Interceptor.attach(sym_aes_decrypt, {
            onEnter: function (args) {
                console.log("\n[NATIVE:aes_decrypt_with_raw_key] =============================");
                // Common signatures: (key, key_len, in, in_len, out, out_len) or (in, in_len, key, key_len, out, out_len)
                // Try to figure out which arg is the key by checking sizes
                var arg0 = args[0];
                var arg1 = args[1].toInt32();
                var arg2 = args[2];
                var arg3 = args[3].toInt32();

                // If arg1 == 16 or 32, arg0 is likely the key
                if (arg1 === 16 || arg1 === 32 || arg1 === 24) {
                    console.log("[NATIVE:aes_decrypt_with_raw_key] key (arg0, " + arg1 + " bytes):");
                    try {
                        var key_buf = new Uint8Array(arg0.readByteArray(arg1));
                        console.log(hexdump_bytes(key_buf));
                        log_key_banner("aes_decrypt_with_raw_key", ptr_to_hex(arg0, arg1));
                    } catch (e) {
                        console.log("  (key read error)");
                    }
                    console.log("[NATIVE:aes_decrypt_with_raw_key] data (arg2, " + arg3 + " bytes)");
                } else if (arg3 === 16 || arg3 === 32 || arg3 === 24) {
                    // Alternative: key is arg2
                    console.log("[NATIVE:aes_decrypt_with_raw_key] data (arg0, " + arg1 + " bytes)");
                    console.log("[NATIVE:aes_decrypt_with_raw_key] key (arg2, " + arg3 + " bytes):");
                    try {
                        var key_buf2 = new Uint8Array(arg2.readByteArray(arg3));
                        console.log(hexdump_bytes(key_buf2));
                        log_key_banner("aes_decrypt_with_raw_key", ptr_to_hex(arg2, arg3));
                    } catch (e) {
                        console.log("  (key read error)");
                    }
                } else {
                    // Dump both args for analysis
                    console.log("[NATIVE:aes_decrypt_with_raw_key] arg0 ptr=" + arg0 + " arg1(int)=" + arg1);
                    console.log("[NATIVE:aes_decrypt_with_raw_key] arg2 ptr=" + arg2 + " arg3(int)=" + arg3);
                    // Try reading 16 bytes from each pointer
                    console.log("[NATIVE:aes_decrypt_with_raw_key] arg0 first 16 bytes: " + ptr_to_hex(arg0, 16));
                    console.log("[NATIVE:aes_decrypt_with_raw_key] arg2 first 16 bytes: " + ptr_to_hex(arg2, 16));
                }
                console.log("[NATIVE:aes_decrypt_with_raw_key] =============================");
            }
        });
        console.log("[+] Hooked aes_decrypt_with_raw_key @ " + sym_aes_decrypt);
    } else {
        console.log("[-] aes_decrypt_with_raw_key not found in " + mod_name);
    }

    // --- 6a. imm_p2p_rtc_sdp_set_aes_key ---
    // Likely: int imm_p2p_rtc_sdp_set_aes_key(void *sdp_ctx, const uint8_t *key, int key_len)
    var sym_sdp_set = Module.findExportByName(mod_name, "imm_p2p_rtc_sdp_set_aes_key");
    if (sym_sdp_set) {
        Interceptor.attach(sym_sdp_set, {
            onEnter: function (args) {
                console.log("\n[NATIVE:sdp_set_aes_key] ======================================");
                console.log("[NATIVE:sdp_set_aes_key] ctx = " + args[0]);
                var key_ptr = args[1];
                var key_len = args[2].toInt32();
                if (key_len <= 0 || key_len > 64) {
                    // Maybe key_len is not the third arg; try reading as c-string
                    console.log("[NATIVE:sdp_set_aes_key] arg1 as string = " + read_cstring(key_ptr));
                    console.log("[NATIVE:sdp_set_aes_key] arg1 first 32 bytes: " + ptr_to_hex(key_ptr, 32));
                    console.log("[NATIVE:sdp_set_aes_key] arg2 = " + args[2]);
                } else {
                    console.log("[NATIVE:sdp_set_aes_key] key (" + key_len + " bytes):");
                    try {
                        var kb = new Uint8Array(key_ptr.readByteArray(key_len));
                        console.log(hexdump_bytes(kb));
                        log_key_banner("sdp_set_aes_key", ptr_to_hex(key_ptr, key_len));
                    } catch (e) {
                        console.log("  (read error)");
                    }
                }
                console.log("[NATIVE:sdp_set_aes_key] ======================================");
            }
        });
        console.log("[+] Hooked imm_p2p_rtc_sdp_set_aes_key @ " + sym_sdp_set);
    }

    // --- 6b. imm_p2p_rtc_sdp_get_aes_key ---
    // Likely: int imm_p2p_rtc_sdp_get_aes_key(void *sdp_ctx, uint8_t *key_out, int *key_len)
    var sym_sdp_get = Module.findExportByName(mod_name, "imm_p2p_rtc_sdp_get_aes_key");
    if (sym_sdp_get) {
        Interceptor.attach(sym_sdp_get, {
            onEnter: function (args) {
                this.ctx = args[0];
                this.key_out = args[1];
                this.key_len_ptr = args[2];
                console.log("\n[NATIVE:sdp_get_aes_key] onEnter ==============================");
                console.log("[NATIVE:sdp_get_aes_key] ctx = " + this.ctx);
            },
            onLeave: function (retval) {
                console.log("[NATIVE:sdp_get_aes_key] onLeave ==============================");
                console.log("[NATIVE:sdp_get_aes_key] retval = " + retval);
                if (!this.key_out.isNull()) {
                    // Try reading key_len from the pointer
                    var key_len = 16; // default assumption for AES-128
                    try {
                        if (!this.key_len_ptr.isNull()) {
                            key_len = this.key_len_ptr.readInt();
                            console.log("[NATIVE:sdp_get_aes_key] key_len (from ptr) = " + key_len);
                        }
                    } catch (e) {
                        // key_len_ptr might be an int, not a pointer
                        key_len = this.key_len_ptr.toInt32();
                        if (key_len <= 0 || key_len > 64) key_len = 16;
                        console.log("[NATIVE:sdp_get_aes_key] key_len (as int) = " + key_len);
                    }
                    console.log("[NATIVE:sdp_get_aes_key] key (" + key_len + " bytes):");
                    try {
                        var kb = new Uint8Array(this.key_out.readByteArray(key_len));
                        console.log(hexdump_bytes(kb));
                        log_key_banner("sdp_get_aes_key", ptr_to_hex(this.key_out, key_len));
                    } catch (e) {
                        console.log("  (read error)");
                    }
                    // Also try as string
                    console.log("[NATIVE:sdp_get_aes_key] key as string = " + read_cstring(this.key_out, 64));
                }
                console.log("[NATIVE:sdp_get_aes_key] ======================================");
            }
        });
        console.log("[+] Hooked imm_p2p_rtc_sdp_get_aes_key @ " + sym_sdp_get);
    }

    // --- Also hook imm_p2p_rtc_sdp_decode and sdp_encode for SDP analysis ---
    var sym_sdp_decode = Module.findExportByName(mod_name, "imm_p2p_rtc_sdp_decode");
    if (sym_sdp_decode) {
        Interceptor.attach(sym_sdp_decode, {
            onEnter: function (args) {
                console.log("\n[NATIVE:sdp_decode] ctx=" + args[0] + " sdp_str=" + read_cstring(args[1], 2048));
            }
        });
        console.log("[+] Hooked imm_p2p_rtc_sdp_decode @ " + sym_sdp_decode);
    }

    return true;
}

// ---------------------------------------------------------------------------
// SECTION 4: Delayed module loading (for spawn mode)
// ---------------------------------------------------------------------------

function wait_for_module(mod_name, hook_fn, description) {
    if (hook_fn()) return; // Already loaded

    console.log("[*] Waiting for " + mod_name + " to load (" + description + ")...");
    var interval = setInterval(function () {
        if (Process.findModuleByName(mod_name)) {
            clearInterval(interval);
            console.log("[*] " + mod_name + " loaded! Installing hooks...");
            try {
                hook_fn();
            } catch (e) {
                console.log("[-] Error hooking " + mod_name + ": " + e.message);
            }
        }
    }, 500);
}

// ---------------------------------------------------------------------------
// SECTION 5: Bonus - hook javax.crypto.Cipher for broad AES coverage
// ---------------------------------------------------------------------------

function hook_javax_crypto() {
    Java.perform(function () {
        try {
            var Cipher = Java.use("javax.crypto.Cipher");
            Cipher.init.overload("int", "java.security.Key", "java.security.spec.AlgorithmParameterSpec").implementation = function (mode, key, params) {
                var algo = this.getAlgorithm();
                if (algo && algo.toUpperCase().indexOf("AES") !== -1) {
                    var modeStr = (mode === 1) ? "ENCRYPT" : (mode === 2) ? "DECRYPT" : "mode=" + mode;
                    console.log("\n[JAVA:Cipher.init] ============================================");
                    console.log("[JAVA:Cipher.init] algorithm = " + algo);
                    console.log("[JAVA:Cipher.init] mode      = " + modeStr);
                    try {
                        var keyBytes = key.getEncoded();
                        var keyArr = [];
                        for (var i = 0; i < keyBytes.length; i++) keyArr.push(keyBytes[i] & 0xff);
                        console.log("[JAVA:Cipher.init] key (" + keyBytes.length + " bytes):");
                        console.log(hexdump_bytes(keyArr));
                        var keyHex = keyArr.map(function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
                        log_key_banner("Cipher.init " + modeStr + " " + algo, keyHex);
                    } catch (e) {
                        console.log("[JAVA:Cipher.init] (key extraction error: " + e.message + ")");
                    }
                    try {
                        var ivSpec = Java.cast(params, Java.use("javax.crypto.spec.IvParameterSpec"));
                        var ivBytes = ivSpec.getIV();
                        var ivArr = [];
                        for (var j = 0; j < ivBytes.length; j++) ivArr.push(ivBytes[j] & 0xff);
                        console.log("[JAVA:Cipher.init] IV (" + ivBytes.length + " bytes):");
                        console.log(hexdump_bytes(ivArr));
                    } catch (e) {
                        console.log("[JAVA:Cipher.init] (IV extraction: " + e.message + ")");
                    }
                    console.log("[JAVA:Cipher.init] ============================================");
                }
                return this.init(mode, key, params);
            };
            console.log("[+] Hooked javax.crypto.Cipher.init (AES filter)");
        } catch (e) {
            console.log("[-] Failed to hook Cipher.init: " + e.message);
        }

        // Also hook SecretKeySpec constructor to catch key material
        try {
            var SecretKeySpec = Java.use("javax.crypto.spec.SecretKeySpec");
            SecretKeySpec.$init.overload("[B", "java.lang.String").implementation = function (keyBytes, algorithm) {
                if (algorithm && algorithm.toUpperCase().indexOf("AES") !== -1) {
                    var arr = [];
                    for (var i = 0; i < keyBytes.length; i++) arr.push(keyBytes[i] & 0xff);
                    console.log("\n[JAVA:SecretKeySpec] algorithm=" + algorithm + " key (" + keyBytes.length + " bytes):");
                    console.log(hexdump_bytes(arr));
                    if (keyBytes.length === 16 || keyBytes.length === 24 || keyBytes.length === 32) {
                        var hex = arr.map(function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
                        log_key_banner("SecretKeySpec AES", hex);
                    }
                }
                return this.$init(keyBytes, algorithm);
            };
            console.log("[+] Hooked SecretKeySpec constructor (AES filter)");
        } catch (e) {
            console.log("[-] Failed to hook SecretKeySpec: " + e.message);
        }

        // Hook Mac for HMAC operations
        try {
            var Mac = Java.use("javax.crypto.Mac");
            Mac.doFinal.overload("[B").implementation = function (input) {
                var algo = this.getAlgorithm();
                if (algo && algo.indexOf("SHA256") !== -1) {
                    console.log("\n[JAVA:Mac.doFinal] algorithm=" + algo + " input (" + input.length + " bytes)");
                    var arr = [];
                    for (var i = 0; i < input.length; i++) arr.push(input[i] & 0xff);
                    console.log(hexdump_bytes(arr, 64));
                }
                var result = this.doFinal(input);
                if (algo && algo.indexOf("SHA256") !== -1) {
                    var res_arr = [];
                    for (var j = 0; j < result.length; j++) res_arr.push(result[j] & 0xff);
                    console.log("[JAVA:Mac.doFinal] result (" + result.length + " bytes):");
                    console.log(hexdump_bytes(res_arr, 32));
                }
                return result;
            };
            console.log("[+] Hooked Mac.doFinal (HMAC-SHA256 filter)");
        } catch (e) {
            console.log("[-] Failed to hook Mac.doFinal: " + e.message);
        }
    });
}

// ---------------------------------------------------------------------------
// Main entry point
// ---------------------------------------------------------------------------

console.log("\n");
console.log("============================================================");
console.log(" Philips Avent SCD973 - AES Key Extraction via Frida");
console.log(" Target: com.philips.ph.babymonitorplus");
console.log("============================================================");
console.log("[*] Script loaded. Installing hooks...\n");

// Install Java hooks
try {
    hook_java();
    hook_javax_crypto();
} catch (e) {
    console.log("[-] Java hook setup error: " + e.message);
}

// Install native hooks (with delayed loading support)
wait_for_module("libThingCameraSDK.so", hook_native_camera_sdk, "camera SDK native hooks");
wait_for_module("libThingP2PSDK.so", hook_native_p2p_sdk, "P2P SDK native hooks");

console.log("\n[*] Hook installation complete. Start a live preview or playback in the app.");
console.log("[*] Watch for [AES-KEY] banners in the output.\n");
