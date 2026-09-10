package fr.valche5.aiusage.net

import fr.valche5.aiusage.domain.ReportsPayload
import kotlinx.serialization.json.Json
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import java.io.IOException
import java.util.concurrent.TimeUnit

class ApiException(message: String, val status: Int = 0) : IOException(message)

class ApiClient(client: OkHttpClient? = null) {
    var baseUrl: String = DEFAULT_BASE_URL
        set(value) {
            field = value.trim().trimEnd('/')
        }

    var token: String = ""

    private val json = Json { ignoreUnknownKeys = true; isLenient = true }

    private val http: OkHttpClient = (client ?: OkHttpClient.Builder()
        .connectTimeout(15, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .writeTimeout(15, TimeUnit.SECONDS)
        .followRedirects(false)
        .followSslRedirects(false)
        .build()
        ).newBuilder()
        .addInterceptor { chain ->
            val original = chain.request()
            val builder = original.newBuilder()
            if (token.isNotEmpty() && original.header("Authorization") == null) {
                builder.header("Authorization", "Bearer $token")
            }
            chain.proceed(builder.build())
        }
        .build()

    fun connect(token: String): ReportsPayload {
        this.token = token.trim()
        if (this.token.isEmpty()) throw ApiException("jeton manquant")
        return reports()
    }

    fun reports(): ReportsPayload {
        val response = execute(
            Request.Builder()
                .url(url("/api/reports"))
                .header("Accept", "application/json")
                .get()
                .build(),
        )
        if (response.code == 401) throw ApiException("jeton refusé", 401)
        if (response.code !in 200..299) {
            throw ApiException("rapports indisponibles (HTTP ${response.code})", response.code)
        }
        return json.decodeFromString(ReportsPayload.serializer(), response.body)
    }

    fun requestRefresh() {
        val response = execute(
            Request.Builder().url(url("/refresh")).post(okhttp3.FormBody.Builder().build()).build(),
        )
        if (response.code == 401) throw ApiException("jeton refusé", 401)
        if (response.code !in 200..399) {
            throw ApiException("actualisation refusée (HTTP ${response.code})", response.code)
        }
    }

    fun clear() {
        token = ""
    }

    fun openReportsSocket(listener: WebSocketListener): WebSocket {
        if (token.isEmpty()) throw ApiException("jeton manquant")
        val request = Request.Builder().url(websocketUrl("/ws")).build()
        return http.newWebSocket(request, listener)
    }

    fun sendRefresh(socket: WebSocket): Boolean =
        socket.send("""{"type":"refresh"}""")

    private fun url(path: String): HttpUrl = (baseUrl + path).toHttpUrl()

    private fun websocketUrl(path: String): String {
        val wsBase = when {
            baseUrl.startsWith("https://") -> "wss://" + baseUrl.removePrefix("https://")
            baseUrl.startsWith("http://") -> "ws://" + baseUrl.removePrefix("http://")
            else -> "wss://$baseUrl"
        }
        return wsBase + path
    }

    private data class Raw(val code: Int, val body: String)

    private fun execute(request: Request): Raw {
        http.newCall(request).execute().use { response ->
            val body = response.body?.string().orEmpty()
            return Raw(response.code, body)
        }
    }

    companion object {
        const val DEFAULT_BASE_URL = "https://aiusage.tail262be8.ts.net"
    }
}
