/*
 * Frida script: dump ALL WebRTC DataChannel messages (SCTP layer).
 *
 * Instead of filtering, this dumps EVERY message sent/received on the
 * data channel. Use this while triggering lullaby play/stop to capture
 * the exact binary/JSON format.
 *
 * Run:
 *   frida -U -f com.philips.ph.babymonitorplus -l tools/frida_datachannel_dump.js --no-pause
 */

"use strict";

var TAG = "[DATACHANNEL]";

setTimeout(function () {

    console.log(TAG + " Scanning for native modules...");

    var modules = Process.enumerateModules();
    var hooked = 0;

    modules.forEach(function (mod) {
        var name = mod.name.toLowerCase();

        // Hook usrsctp / sctp send functions (WebRTC DataChannel uses SCTP)
        if (name.indexOf("sctp") !== -1 || name.indexOf("usrsctp") !== -1) {
            console.log(TAG + " [SCTP] " + mod.name + " @ " + mod.base);
            mod.enumerateExports().forEach(function (exp) {
                var en = exp.name.toLowerCase();
                if (en.indexOf("send") !== -1 || en.indexOf("write") !== -1) {
                    console.log(TAG + "   -> " + exp.name);
                    try {
                        Interceptor.attach(exp.address, {
                            onEnter: function (args) {
                                // usrsctp_sendv(sock, data, len, ...) or similar
                                var len = args[2] ? args[2].toInt32() : 0;
                                if (len > 0 && len < 65536) {
                                    var buf = args[1];
                                    console.log("\n" + TAG + " [SCTP-SEND] " + exp.name + " len=" + len);
                                    try {
                                        var str = buf.readUtf8String(Math.min(len, 2048));
                                        if (str) console.log(TAG + "   text=" + str);
                                    } catch (e) {}
                                    console.log(TAG + "   hex=" + hexdump_compact(buf, len));
                                }
                            }
                        });
                        hooked++;
                    } catch (e) {}
                }
            });
        }

        // Hook Tuya's own P2P/IPC native lib
        if (name.indexOf("tuya") !== -1 || name.indexOf("thing") !== -1) {
            console.log(TAG + " [TUYA-SO] " + mod.name + " @ " + mod.base);

            var allExports = mod.enumerateExports();
            console.log(TAG + "   Total exports: " + allExports.length);

            allExports.forEach(function (exp) {
                var en = exp.name.toLowerCase();
                // Broad filter: anything that could send data
                if (en.indexOf("send") !== -1 || en.indexOf("write") !== -1 ||
                    en.indexOf("dp") !== -1 || en.indexOf("cmd") !== -1 ||
                    en.indexOf("command") !== -1 || en.indexOf("ctrl") !== -1 ||
                    en.indexOf("channel") !== -1 || en.indexOf("message") !== -1 ||
                    en.indexOf("data") !== -1 || en.indexOf("publish") !== -1) {

                    if (exp.type === "function") {
                        console.log(TAG + "   export: " + exp.name);
                        try {
                            Interceptor.attach(exp.address, {
                                onEnter: function (args) {
                                    console.log("\n" + TAG + " [NATIVE-CALL] " + mod.name + "!" + exp.name);
                                    dump_args(args, 6);
                                },
                                onLeave: function (retval) {
                                    console.log(TAG + "   ret=" + retval);
                                }
                            });
                            hooked++;
                        } catch (e) {}
                    }
                }
            });
        }

        // Hook libwebrtc or peerconnection native
        if (name.indexOf("webrtc") !== -1 || name.indexOf("peerconnection") !== -1 ||
            name.indexOf("jingle") !== -1) {
            console.log(TAG + " [WEBRTC-SO] " + mod.name + " @ " + mod.base);
            mod.enumerateExports().forEach(function (exp) {
                var en = exp.name.toLowerCase();
                if (en.indexOf("send") !== -1 && en.indexOf("data") !== -1) {
                    console.log(TAG + "   -> " + exp.name);
                    try {
                        Interceptor.attach(exp.address, {
                            onEnter: function (args) {
                                console.log("\n" + TAG + " [WEBRTC-SEND] " + exp.name);
                                dump_args(args, 4);
                            }
                        });
                        hooked++;
                    } catch (e) {}
                }
            });
        }
    });

    console.log("\n" + TAG + " Hooked " + hooked + " native functions");

    // Also hook Java-level DataChannel if present
    Java.perform(function () {
        // WebRTC DataChannel.send (org.webrtc)
        try {
            var DataChannel = Java.use("org.webrtc.DataChannel");
            DataChannel.send.implementation = function (buffer) {
                var bb = buffer.data.value;
                var size = bb.remaining();
                var bytes = Java.array("byte", new Array(size));
                bb.get(bytes);
                bb.rewind();

                var str = "";
                try { str = Java.use("java.lang.String").$new(bytes, "UTF-8"); } catch (e) {}
                console.log("\n" + TAG + " [JAVA-DC-SEND] size=" + size + " binary=" + buffer.binary.value);
                if (str) console.log(TAG + "   text=" + str);

                return this.send(buffer);
            };
            console.log(TAG + " [+] Hooked org.webrtc.DataChannel.send");
        } catch (e) {
            console.log(TAG + " [-] org.webrtc.DataChannel not loaded: " + e.message);
        }

        // Tuya's own DataChannel wrapper
        try {
            Java.enumerateLoadedClasses({
                onMatch: function (name) {
                    if (name.indexOf("thingclips") === -1) return;
                    if (name.toLowerCase().indexOf("datachannel") === -1 &&
                        name.toLowerCase().indexOf("data_channel") === -1) return;

                    console.log(TAG + " [TUYA-DC] Found: " + name);
                    try {
                        var cls = Java.use(name);
                        cls.class.getDeclaredMethods().forEach(function (m) {
                            console.log(TAG + "   method: " + m.getName() + " (" + m.getParameterTypes().length + " params)");
                            var mname = m.getName().toLowerCase();
                            if (mname.indexOf("send") !== -1 || mname.indexOf("write") !== -1) {
                                try {
                                    cls[m.getName()].overloads.forEach(function (overload) {
                                        overload.implementation = function () {
                                            console.log("\n" + TAG + " [TUYA-DC-SEND] " + name.split(".").pop() + "." + m.getName());
                                            for (var i = 0; i < arguments.length; i++) {
                                                try { console.log(TAG + "   arg" + i + "=" + JSON.stringify(arguments[i])); }
                                                catch (e) { console.log(TAG + "   arg" + i + "=" + arguments[i]); }
                                            }
                                            return overload.apply(this, arguments);
                                        };
                                    });
                                    console.log(TAG + " [+] Hooked " + name.split(".").pop() + "." + m.getName());
                                } catch (e) {}
                            }
                        });
                    } catch (e) {}
                },
                onComplete: function () {}
            });
        } catch (e) {}
    });

    console.log("\n" + TAG + " === READY ===");
    console.log(TAG + " Trigger lullaby play/stop in the app now.");

}, 3000);

function hexdump_compact(ptr, len) {
    var max = Math.min(len, 128);
    try {
        var arr = new Uint8Array(ptr.readByteArray(max));
        var hex = [];
        for (var i = 0; i < arr.length; i++) {
            hex.push(("0" + arr[i].toString(16)).slice(-2));
        }
        return hex.join(" ") + (len > max ? " ..." : "");
    } catch (e) {
        return "(unreadable)";
    }
}

function dump_args(args, count) {
    for (var i = 0; i < count; i++) {
        try {
            var val = args[i];
            if (val.isNull()) {
                console.log(TAG + "   arg" + i + " = NULL");
                continue;
            }
            // Try as int
            var intVal = val.toInt32();
            // Try as string
            try {
                var str = val.readUtf8String(256);
                if (str && str.length > 1 && str.length < 256) {
                    console.log(TAG + "   arg" + i + " = " + intVal + " / str=\"" + str + "\"");
                    continue;
                }
            } catch (e) {}
            // Try as buffer
            if (intVal > 256) {
                try {
                    var hex = hexdump_compact(val, 64);
                    console.log(TAG + "   arg" + i + " = " + val + " hex=[" + hex + "]");
                    continue;
                } catch (e) {}
            }
            console.log(TAG + "   arg" + i + " = " + val + " (int=" + intVal + ")");
        } catch (e) {
            console.log(TAG + "   arg" + i + " = (error: " + e + ")");
        }
    }
}
