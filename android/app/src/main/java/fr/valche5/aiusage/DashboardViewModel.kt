package fr.valche5.aiusage

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import fr.valche5.aiusage.data.Prefs
import fr.valche5.aiusage.domain.ReportsPayload
import fr.valche5.aiusage.net.ApiClient
import fr.valche5.aiusage.net.ApiException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import java.util.concurrent.atomic.AtomicBoolean

data class UiState(
    val loading: Boolean = false,
    val loggedIn: Boolean = false,
    val baseUrl: String = ApiClient.DEFAULT_BASE_URL,
    val token: String = "",
    val remember: Boolean = true,
    val error: String? = null,
    val reports: ReportsPayload? = null,
    val live: Boolean = false,
    val refreshing: Boolean = false,
)

class DashboardViewModel(application: Application) : AndroidViewModel(application) {
    private val prefs = Prefs(application)
    private val api = ApiClient()
    private val json = Json { ignoreUnknownKeys = true; isLenient = true }
    private var socket: WebSocket? = null
    private var reconnect: Job? = null
    private val closing = AtomicBoolean(false)

    private val _state = MutableStateFlow(
        UiState(
            baseUrl = prefs.baseUrl,
            token = prefs.token,
            remember = prefs.rememberToken,
        ),
    )
    val state: StateFlow<UiState> = _state

    init {
        api.baseUrl = prefs.baseUrl
        if (prefs.token.isNotEmpty()) {
            connect(prefs.token, prefs.baseUrl, prefs.rememberToken)
        }
    }

    fun connect(token: String, baseUrl: String = _state.value.baseUrl, remember: Boolean = _state.value.remember) {
        viewModelScope.launch {
            _state.update {
                it.copy(loading = true, error = null, token = token, baseUrl = baseUrl, remember = remember)
            }
            try {
                withContext(Dispatchers.IO) {
                    api.baseUrl = baseUrl
                    val reports = api.connect(token)
                    prefs.baseUrl = baseUrl
                    prefs.rememberToken = remember
                    if (remember) prefs.token = token else prefs.clearToken()
                    reports
                }.also { reports ->
                    _state.update {
                        it.copy(loading = false, loggedIn = true, reports = reports, error = null)
                    }
                    connectSocket()
                }
            } catch (e: Exception) {
                api.clear()
                _state.update {
                    it.copy(loading = false, loggedIn = false, error = e.message ?: "connexion impossible")
                }
            }
        }
    }

    fun refresh() {
        val current = _state.value
        if (!current.loggedIn || current.refreshing) return
        viewModelScope.launch {
            _state.update { it.copy(refreshing = true, error = null) }
            try {
                val sent = socket?.let { api.sendRefresh(it) } == true
                if (!sent) {
                    val reports = withContext(Dispatchers.IO) {
                        api.requestRefresh()
                        api.reports()
                    }
                    _state.update { it.copy(reports = reports, refreshing = false) }
                } else {
                    delay(12_000)
                    _state.update { it.copy(refreshing = false) }
                }
            } catch (e: ApiException) {
                if (e.status == 401) {
                    sessionLost()
                } else {
                    _state.update { it.copy(refreshing = false, error = e.message) }
                }
            } catch (e: Exception) {
                _state.update { it.copy(refreshing = false, error = e.message) }
            }
        }
    }

    fun logout() {
        viewModelScope.launch {
            closing.set(true)
            reconnect?.cancel()
            socket?.cancel()
            socket = null
            api.clear()
            prefs.clearToken()
            closing.set(false)
            _state.update {
                it.copy(
                    loggedIn = false,
                    reports = null,
                    live = false,
                    refreshing = false,
                    token = "",
                    error = null,
                )
            }
        }
    }

    private fun connectSocket() {
        reconnect?.cancel()
        closing.set(false)
        reconnect = viewModelScope.launch {
            var backoff = 1_000L
            while (isActive && !closing.get() && _state.value.loggedIn) {
                try {
                    val ws = withContext(Dispatchers.IO) {
                        api.openReportsSocket(object : WebSocketListener() {
                            override fun onOpen(webSocket: WebSocket, response: Response) {
                                _state.update { it.copy(live = true) }
                            }

                            override fun onMessage(webSocket: WebSocket, text: String) {
                                runCatching { json.decodeFromString(ReportsPayload.serializer(), text) }
                                    .onSuccess { payload ->
                                        if (payload.providers.isNotEmpty() || payload.type == "reports") {
                                            _state.update {
                                                it.copy(reports = payload, refreshing = false, error = null)
                                            }
                                        }
                                    }
                            }

                            override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                                webSocket.close(code, reason)
                            }

                            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                                _state.update { it.copy(live = false) }
                            }

                            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                                _state.update { it.copy(live = false) }
                            }
                        })
                    }
                    socket?.cancel()
                    socket = ws
                    backoff = 1_000L
                    while (isActive && !closing.get() && _state.value.live) {
                        delay(1_000)
                    }
                } catch (e: ApiException) {
                    if (e.status == 401) {
                        sessionLost()
                        return@launch
                    }
                } catch (_: Exception) {
                    _state.update { it.copy(live = false) }
                }
                if (closing.get() || !_state.value.loggedIn) return@launch
                delay(backoff)
                backoff = (backoff * 2).coerceAtMost(15_000L)
            }
        }
    }

    private fun sessionLost() {
        closing.set(true)
        socket?.cancel()
        socket = null
        api.clear()
        _state.update {
            it.copy(
                loggedIn = false,
                live = false,
                refreshing = false,
                reports = null,
                error = "jeton refusé — vérifie le Bearer",
            )
        }
    }

    override fun onCleared() {
        closing.set(true)
        reconnect?.cancel()
        socket?.cancel()
        super.onCleared()
    }
}
