package fr.valche5.aiusage.data

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import fr.valche5.aiusage.net.ApiClient

class Prefs(context: Context) {
    private val prefs: SharedPreferences = runCatching {
        val master = MasterKey.Builder(context)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        EncryptedSharedPreferences.create(
            context,
            "ai-usage-secure",
            master,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }.getOrElse {
        context.getSharedPreferences("ai-usage", Context.MODE_PRIVATE)
    }

    var baseUrl: String
        get() = prefs.getString(KEY_URL, ApiClient.DEFAULT_BASE_URL)?.ifBlank { ApiClient.DEFAULT_BASE_URL }
            ?: ApiClient.DEFAULT_BASE_URL
        set(value) {
            prefs.edit().putString(KEY_URL, value.trim().trimEnd('/')).apply()
        }

    var token: String
        get() = prefs.getString(KEY_TOKEN, null)
            ?: prefs.getString(KEY_PASSWORD, "").orEmpty()
        set(value) {
            prefs.edit().putString(KEY_TOKEN, value).remove(KEY_PASSWORD).apply()
        }

    var rememberToken: Boolean
        get() = prefs.getBoolean(KEY_REMEMBER, true)
        set(value) {
            prefs.edit().putBoolean(KEY_REMEMBER, value).apply()
        }

    fun clearToken() {
        prefs.edit().remove(KEY_TOKEN).remove(KEY_PASSWORD).apply()
    }

    companion object {
        private const val KEY_URL = "base_url"
        private const val KEY_TOKEN = "token"
        private const val KEY_PASSWORD = "password"
        private const val KEY_REMEMBER = "remember"
    }
}
