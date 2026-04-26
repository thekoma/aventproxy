/*
 * Frida script: intercept lullaby commands at ALL layers.
 *
 * Run:
 *   frida -U -f com.philips.ph.babymonitorplus -l tools/frida_lullaby_intercept.js --no-pause
 *
 * Then trigger play/stop in the app and watch the output.
 *
 * Hooks:
 *   1. Java MQTT publish (paho + Tuya wrappers)
 *   2. Java P2P SDK methods (ThingP2PSDK, ICameraP2P, DpHelper)
 *   3. Native libc send()/sendto()/write() filtered for JSON/DPS patterns
 *   4. Native libtuya*.so exported functions
 */

"use strict";

var TAG = "[LULLABY-RE]";
var FILTER_KEYWORDS = ["201", "play", "stop", "pause", "next", "prev",
                        "lullaby", "music", "dps", "dp_publish", "play_control"];

function containsKeyword(s) {
    var lower = s.toLowerCase();
    for (var i = 0; i < FILTER_KEYWORDS.length; i++) {
        if (lower.indexOf(FILTER_KEYWORDS[i]) !== -1) return true;
    }
    return false;
}

function hexdump_short(buf, len) {
    var max = Math.min(len, 512);
    var arr = new Uint8Array(buf.slice(0, max));
    var hex = [];
    var ascii = [];
    for (var i = 0; i < arr.length; i++) {
        hex.push(("0" + arr[i].toString(16)).slice(-2));
        ascii.push(arr[i] >= 32 && arr[i] < 127 ? String.fromCharCode(arr[i]) : ".");
    }
    return hex.join(" ") + "\n" + ascii.join("");
}

setTimeout(function () {
    Java.perform(function () {
        console.log(TAG + " === LAYER 1: Java MQTT publish ===");

        // Hook paho MqttAsyncClient.publish
        try {
            var MqttAsyncClient = Java.use("org.eclipse.paho.client.mqttv3.MqttAsyncClient");
            MqttAsyncClient.publish.overloads.forEach(function (overload) {
                overload.implementation = function () {
                    var topic = arguments[0];
                    var topicStr = topic ? topic.toString() : "";
                    var payload = "";
                    try {
                        if (arguments[1] && arguments[1].getPayload) {
                            var bytes = arguments[1].getPayload();
                            payload = Java.use("java.lang.String").$new(bytes, "UTF-8");
                        } else if (arguments[1] instanceof Array || arguments[1].$className === "[B") {
                            payload = Java.use("java.lang.String").$new(arguments[1], "UTF-8");
                        }
                    } catch (e) {}

                    if (topicStr.indexOf("/av/") !== -1 || containsKeyword(payload) || containsKeyword(topicStr)) {
                        console.log("\n" + TAG + " [MQTT-PUB] topic=" + topicStr);
                        console.log(TAG + " payload=" + payload);
                        console.log(TAG + " stack=" + Java.use("android.util.Log").getStackTraceString(Java.use("java.lang.Exception").$new()));
                    }
                    return overload.apply(this, arguments);
                };
            });
            console.log(TAG + " [+] Hooked paho MqttAsyncClient.publish");
        } catch (e) {
            console.log(TAG + " [-] paho not found: " + e);
        }

        // Hook Tuya MQTT wrapper
        try {
            Java.enumerateLoadedClasses({
                onMatch: function (name) {
                    if (name.indexOf("thingclips") === -1) return;
                    if (name.indexOf("mqtt") === -1 && name.indexOf("Mqtt") === -1) return;
                    try {
                        var cls = Java.use(name);
                        var methods = cls.class.getDeclaredMethods();
                        methods.forEach(function (m) {
                            var mname = m.getName().toLowerCase();
                            if (mname.indexOf("publish") !== -1 || mname.indexOf("send") !== -1) {
                                try {
                                    cls[m.getName()].overloads.forEach(function (overload) {
                                        overload.implementation = function () {
                                            var args_str = [];
                                            for (var i = 0; i < arguments.length; i++) {
                                                try { args_str.push(JSON.stringify(arguments[i])); }
                                                catch (e2) { args_str.push(String(arguments[i])); }
                                            }
                                            console.log("\n" + TAG + " [TUYA-MQTT] " + name.split(".").pop() + "." + m.getName());
                                            console.log(TAG + " args: " + args_str.join(", "));
                                            return overload.apply(this, arguments);
                                        };
                                    });
                                    console.log(TAG + " [+] " + name.split(".").pop() + "." + m.getName());
                                } catch (e3) {}
                            }
                        });
                    } catch (e4) {}
                },
                onComplete: function () {}
            });
        } catch (e) {
            console.log(TAG + " [-] Tuya MQTT scan error: " + e);
        }

        console.log(TAG + " === LAYER 2: Java P2P SDK ===");

        // Hook ALL methods on P2P-related classes
        var p2pClassPatterns = ["p2p", "P2P", "DataChannel", "datachannel", "DpHelper", "MusicManager", "music"];
        Java.enumerateLoadedClasses({
            onMatch: function (name) {
                if (name.indexOf("thingclips") === -1) return;
                var nameLower = name.toLowerCase();
                var isP2P = false;
                for (var i = 0; i < p2pClassPatterns.length; i++) {
                    if (nameLower.indexOf(p2pClassPatterns[i].toLowerCase()) !== -1) {
                        isP2P = true;
                        break;
                    }
                }
                if (!isP2P) return;

                try {
                    var cls = Java.use(name);
                    var methods = cls.class.getDeclaredMethods();
                    methods.forEach(function (m) {
                        var mname = m.getName().toLowerCase();
                        if (mname.indexOf("send") !== -1 || mname.indexOf("write") !== -1 ||
                            mname.indexOf("publish") !== -1 || mname.indexOf("dispatch") !== -1 ||
                            mname.indexOf("command") !== -1 || mname.indexOf("control") !== -1 ||
                            mname.indexOf("play") !== -1 || mname.indexOf("stop") !== -1 ||
                            mname.indexOf("music") !== -1 || mname.indexOf("lullaby") !== -1 ||
                            mname.indexOf("dp") !== -1 || mname.indexOf("dps") !== -1) {
                            try {
                                cls[m.getName()].overloads.forEach(function (overload) {
                                    overload.implementation = function () {
                                        var args_str = [];
                                        for (var ii = 0; ii < arguments.length; ii++) {
                                            try { args_str.push(JSON.stringify(arguments[ii])); }
                                            catch (e5) { args_str.push(String(arguments[ii])); }
                                        }
                                        console.log("\n" + TAG + " [P2P-JAVA] " + name.split(".").pop() + "." + m.getName());
                                        console.log(TAG + " args: " + args_str.join(", "));
                                        console.log(TAG + " stack=" + Java.use("android.util.Log").getStackTraceString(Java.use("java.lang.Exception").$new()));
                                        return overload.apply(this, arguments);
                                    };
                                });
                                console.log(TAG + " [+] " + name.split(".").pop() + "." + m.getName());
                            } catch (e6) {}
                        }
                    });
                } catch (e7) {}
            },
            onComplete: function () {}
        });

        console.log(TAG + " === LAYER 3: JNI native method registration ===");

        // Hook RegisterNatives to catch native method bindings
        var artModule = Process.findModuleByName("libart.so");
        if (artModule) {
            var registerNatives = Module.findExportByName("libart.so", "art_jni_RegisterNatives") ||
                                   Module.findExportByName("libart.so", "_ZN3art3JNI15RegisterNativesEP7_JNIEnvP7_jclassPK15JNINativeMethodi");
            // Skip if not found - not critical
            if (registerNatives) {
                console.log(TAG + " [+] Found RegisterNatives at " + registerNatives);
            }
        }
    });

    console.log(TAG + " === LAYER 4: Native socket send() ===");

    // Hook libc send() - filter for JSON/DPS-like payloads
    var sendPtr = Module.findExportByName("libc.so", "send");
    if (sendPtr) {
        Interceptor.attach(sendPtr, {
            onEnter: function (args) {
                this.fd = args[0].toInt32();
                this.buf = args[1];
                this.len = args[2].toInt32();

                if (this.len > 10 && this.len < 65536) {
                    try {
                        var data = this.buf.readUtf8String(Math.min(this.len, 2048));
                        if (data && containsKeyword(data)) {
                            console.log("\n" + TAG + " [SEND] fd=" + this.fd + " len=" + this.len);
                            console.log(TAG + " data=" + data.substring(0, 1024));
                        }
                    } catch (e) {
                        // binary data, check for JSON-like patterns
                        try {
                            var bytes = new Uint8Array(this.buf.readByteArray(Math.min(this.len, 512)));
                            var ascii = "";
                            for (var i = 0; i < bytes.length; i++) {
                                if (bytes[i] >= 32 && bytes[i] < 127) ascii += String.fromCharCode(bytes[i]);
                            }
                            if (containsKeyword(ascii)) {
                                console.log("\n" + TAG + " [SEND-BIN] fd=" + this.fd + " len=" + this.len);
                                console.log(TAG + " ascii=" + ascii.substring(0, 1024));
                            }
                        } catch (e2) {}
                    }
                }
            }
        });
        console.log(TAG + " [+] Hooked libc send()");
    }

    // Hook sendto() for UDP (WebRTC uses UDP)
    var sendtoPtr = Module.findExportByName("libc.so", "sendto");
    if (sendtoPtr) {
        Interceptor.attach(sendtoPtr, {
            onEnter: function (args) {
                this.fd = args[0].toInt32();
                this.buf = args[1];
                this.len = args[2].toInt32();

                if (this.len > 10 && this.len < 65536) {
                    try {
                        var data = this.buf.readUtf8String(Math.min(this.len, 2048));
                        if (data && containsKeyword(data)) {
                            console.log("\n" + TAG + " [SENDTO] fd=" + this.fd + " len=" + this.len);
                            console.log(TAG + " data=" + data.substring(0, 1024));
                        }
                    } catch (e) {}
                }
            }
        });
        console.log(TAG + " [+] Hooked libc sendto()");
    }

    console.log(TAG + " === LAYER 5: Tuya native .so libraries ===");

    // Find and hook Tuya native libraries
    var modules = Process.enumerateModules();
    modules.forEach(function (mod) {
        var name = mod.name.toLowerCase();
        if (name.indexOf("tuya") !== -1 || name.indexOf("thing") !== -1 ||
            name.indexOf("p2p") !== -1 || name.indexOf("ipc") !== -1 ||
            name.indexOf("webrtc") !== -1 || name.indexOf("datachannel") !== -1 ||
            name.indexOf("sctp") !== -1 || name.indexOf("dtls") !== -1) {

            console.log(TAG + " [SO] " + mod.name + " @ " + mod.base + " (" + mod.size + " bytes)");

            // Enumerate exports looking for send/write/dp/command functions
            var exports = mod.enumerateExports();
            exports.forEach(function (exp) {
                var expLower = exp.name.toLowerCase();
                if (expLower.indexOf("send") !== -1 || expLower.indexOf("write") !== -1 ||
                    expLower.indexOf("command") !== -1 || expLower.indexOf("dp") !== -1 ||
                    expLower.indexOf("publish") !== -1 || expLower.indexOf("dispatch") !== -1 ||
                    expLower.indexOf("music") !== -1 || expLower.indexOf("play") !== -1 ||
                    expLower.indexOf("control") !== -1) {

                    console.log(TAG + "   export: " + exp.name + " @ " + exp.address);

                    if (exp.type === "function") {
                        try {
                            Interceptor.attach(exp.address, {
                                onEnter: function (args) {
                                    console.log("\n" + TAG + " [NATIVE] " + mod.name + "!" + exp.name);
                                    // Dump first 4 args
                                    for (var a = 0; a < 4; a++) {
                                        try {
                                            var val = args[a];
                                            // Try as string
                                            try {
                                                var str = val.readUtf8String(256);
                                                if (str && str.length > 1) {
                                                    console.log(TAG + "   arg" + a + " (str)=" + str.substring(0, 256));
                                                    continue;
                                                }
                                            } catch (e) {}
                                            console.log(TAG + "   arg" + a + " = " + val);
                                        } catch (e) {
                                            console.log(TAG + "   arg" + a + " = (unreadable)");
                                        }
                                    }
                                },
                                onLeave: function (retval) {
                                    console.log(TAG + "   ret=" + retval);
                                }
                            });
                        } catch (e) {
                            console.log(TAG + "   (skip hook: " + e + ")");
                        }
                    }
                }
            });
        }
    });

    console.log("\n" + TAG + " === ALL HOOKS INSTALLED ===");
    console.log(TAG + " Now trigger play/stop lullaby in the app.");
    console.log(TAG + " Watch for output across all 5 layers.");

}, 3000);
