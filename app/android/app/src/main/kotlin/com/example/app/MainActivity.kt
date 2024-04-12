package com.example.app

import androidx.annotation.NonNull
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel

class MainActivity: FlutterActivity() {
    private val CHANNEL = "com.github.codingeasygo/bsrouter"

    override fun configureFlutterEngine(@NonNull flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, CHANNEL).setMethodCallHandler {
            call, result ->
            when (call.method) {
                "start" -> {
                    bsrouter.Bsrouter.bootstrap(applicationInfo.dataDir,MainLog())
                    bsrouter.Bsrouter.startService(call.argument("config"))
                    result.success("OK")
                }
                "name" -> {
                    result.success(bsrouter.Bsrouter.name())
                }
                "state" -> {
                    result.success(bsrouter.Bsrouter.state())
                }
                else -> {
                    result.notImplemented()
                }
            }
        }
    }
}
