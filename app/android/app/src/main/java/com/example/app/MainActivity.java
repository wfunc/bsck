package com.example.app;

import android.content.Intent;
import android.util.Log;

import androidx.annotation.NonNull;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;

import bsrouter.Bsrouter;
import io.flutter.embedding.android.FlutterActivity;
import io.flutter.embedding.engine.FlutterEngine;
import io.flutter.plugin.common.MethodCall;
import io.flutter.plugin.common.MethodChannel;

public class MainActivity extends FlutterActivity {

    @Override
    public void configureFlutterEngine(@NonNull FlutterEngine flutterEngine) {
        super.configureFlutterEngine(flutterEngine);
        new MethodChannel(flutterEngine.getDartExecutor().getBinaryMessenger(), "bsrouter").setMethodCallHandler(bsrouter);
    }

    MethodChannel.MethodCallHandler bsrouter = new MethodChannel.MethodCallHandler() {

        @Override
        public void onMethodCall(@NonNull MethodCall call, @NonNull MethodChannel.Result result) {
            String configFilePath = MainActivity.this.getApplicationInfo().dataDir + "/.bsrouter.json";
            File configFile = new File(configFilePath);

            if (!configFile.exists()) {
                try {
                    writeDefaultConfig(configFile);
                } catch (IOException e) {
                    Log.e("MainActivity", "Failed to write default config", e);
                    result.error("FILE_WRITE_ERROR", "Failed to write default config", e);
                    return;
                }
            }

            Intent intent = new Intent(MainActivity.this, RouterService.class);
            switch (call.method) {
                case "state":
                    result.success(Bsrouter.state(configFilePath));
                    break;
                case "start":
                    MainActivity.this.startService(intent);
                    result.success("Service started");
                    break;
                case "stop":
                    MainActivity.this.stopService(intent);
                    result.success("Service stopped");
                    break;
                default:
                    result.notImplemented();
                    break;
            }
        }

        private void writeDefaultConfig(File file) throws IOException {
            String defaultConfig = "{\"name\":\"phone1\",\"listen\":\"\",\"cert\":\"bsrouter.pem\",\"key\":\"bsrouter.key\",\"console\":{},\"acl\":{},\"access\":[[\".*\",\".*\"]],\"dialer\":{\"std\":1,\"ssh\":1},\"forwards\":{},\"channels\":{\"hk253\":{\"enable\":1,\"remote\":\"tls://159.138.155.33:31103\",\"token\":\"Abc123\",\"tls_verify\":0},\"xjp4\":{\"enable\":1,\"remote\":\"tls://190.92.213.4:31103\",\"token\":\"Abc123\",\"tls_verify\":0}},\"showlog\":0,\"logflags\":16}";

            try (FileOutputStream fos = new FileOutputStream(file)) {
                fos.write(defaultConfig.getBytes());
            }
        }
    };
}
