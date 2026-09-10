package fr.valche5.aiusage

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.Surface
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import fr.valche5.aiusage.ui.AiUsageTheme
import fr.valche5.aiusage.ui.Bg
import fr.valche5.aiusage.ui.DashboardScreen
import fr.valche5.aiusage.ui.LoginScreen

class MainActivity : ComponentActivity() {
    private val viewModel: DashboardViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            AiUsageTheme {
                val state by viewModel.state.collectAsState()
                Surface(modifier = Modifier.fillMaxSize().background(Bg), color = Bg) {
                    if (state.loggedIn) {
                        DashboardScreen(
                            state = state,
                            onRefresh = viewModel::refresh,
                            onLogout = viewModel::logout,
                        )
                    } else {
                        LoginScreen(state = state, onConnect = viewModel::connect)
                    }
                }
            }
        }
    }
}
