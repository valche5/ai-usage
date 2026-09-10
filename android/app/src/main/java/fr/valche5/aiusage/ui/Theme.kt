package fr.valche5.aiusage.ui

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

val Bg = Color(0xFF0D1117)
val Card = Color(0xFF161B22)
val Line = Color(0xFF30363D)
val Text = Color(0xFFE6EDF3)
val Muted = Color(0xFF8B949E)
val Accent = Color(0xFF58A6FF)
val Ok = Color(0xFF3FB950)
val Warn = Color(0xFFD29922)
val Bad = Color(0xFFF85149)
val GreenBtn = Color(0xFF238636)
val SurfaceAlt = Color(0xFF21262D)

private val scheme = darkColorScheme(
    primary = Accent,
    onPrimary = Color.White,
    background = Bg,
    onBackground = Text,
    surface = Card,
    onSurface = Text,
    surfaceVariant = SurfaceAlt,
    onSurfaceVariant = Muted,
    outline = Line,
    error = Bad,
    onError = Color.White,
    secondary = GreenBtn,
    onSecondary = Color.White,
)

@Composable
fun AiUsageTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = scheme, content = content)
}
