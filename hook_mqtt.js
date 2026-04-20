Java.perform(function() {
    // Hook MQTT client connection to see credentials
    var MqttConnectOptions = Java.use("org.eclipse.paho.client.mqttv3.MqttConnectOptions");

    MqttConnectOptions.setUserName.implementation = function(username) {
        console.log("[MQTT] username: " + username);
        return this.setUserName(username);
    };

    MqttConnectOptions.setPassword.implementation = function(password) {
        var pw = "";
        var arr = Java.array("char", password);
        for (var i = 0; i < arr.length; i++) pw += arr[i];
        console.log("[MQTT] password: " + pw);
        return this.setPassword(password);
    };

    // Hook the MQTT client connect
    var MqttAsyncClient = Java.use("org.eclipse.paho.client.mqttv3.MqttAsyncClient");
    MqttAsyncClient.$init.overloads.forEach(function(overload) {
        overload.implementation = function() {
            if (arguments.length >= 2) {
                console.log("[MQTT] broker URL: " + arguments[0]);
                console.log("[MQTT] client ID: " + arguments[1]);
            }
            return overload.apply(this, arguments);
        };
    });

    console.log("[*] MQTT hooks ready");
});
