/*
 * Cast a wide net - hook ALL publishDps/sendDps across the entire SDK
 */
setTimeout(function () {
    Java.perform(function () {
        console.log("--- Wide DPS hook ---");

        // Find ALL classes with publish/send + dp/Dp methods
        var targets = [];
        Java.enumerateLoadedClasses({
            onMatch: function(name) {
                if (name.indexOf("thingclips") === -1) return;
                try {
                    var cls = Java.use(name);
                    var methods = cls.class.getDeclaredMethods();
                    methods.forEach(function(m) {
                        var mname = m.getName().toLowerCase();
                        if ((mname.indexOf("publish") !== -1 || mname.indexOf("send") !== -1) &&
                            (mname.indexOf("dp") !== -1)) {
                            targets.push({cls: name, method: m.getName()});
                        }
                    });
                } catch(e) {}
            },
            onComplete: function() {}
        });

        console.log("[i] Found " + targets.length + " publishDp/sendDp methods:");
        targets.forEach(function(t) {
            console.log("    " + t.cls + "." + t.method);
        });

        // Hook each one
        targets.forEach(function(t) {
            try {
                var cls = Java.use(t.cls);
                cls[t.method].overloads.forEach(function(overload) {
                    overload.implementation = function() {
                        console.log("\n[DPS CALL] " + t.cls.split(".").pop() + "." + t.method);
                        for (var i = 0; i < arguments.length; i++) {
                            try {
                                console.log("  arg" + i + " = " + JSON.stringify(arguments[i]));
                            } catch(e) {
                                console.log("  arg" + i + " = " + arguments[i]);
                            }
                        }
                        console.log("---");
                        return overload.apply(this, arguments);
                    };
                });
                console.log("[+] " + t.cls.split(".").pop() + "." + t.method);
            } catch(e) {}
        });

        // Also hook the native P2P command sender
        try {
            Java.enumerateLoadedClasses({
                onMatch: function(name) {
                    if (name.indexOf("thingclips") === -1) return;
                    try {
                        var cls = Java.use(name);
                        var methods = cls.class.getDeclaredMethods();
                        methods.forEach(function(m) {
                            var mname = m.getName().toLowerCase();
                            if (mname === "sendcommand" || mname === "send_command" ||
                                mname === "sendmessage" || mname === "senddata" ||
                                mname === "writedp" || mname === "setdp") {
                                console.log("[i] Cmd: " + name.split(".").pop() + "." + m.getName());
                            }
                        });
                    } catch(e) {}
                },
                onComplete: function() {}
            });
        } catch(e) {}
    });
}, 1000);
