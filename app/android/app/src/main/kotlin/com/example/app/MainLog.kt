package com.example.app

import android.util.Log

class MainLog :bsrouter.Logger {
    override fun printLog(p0: String?) {
        p0?.let { Log.i("bsrouter", it) }
    }
}