package fr.valche5.aiusage.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding

import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.Logout
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import fr.valche5.aiusage.UiState
import fr.valche5.aiusage.domain.ReportDto
import fr.valche5.aiusage.domain.Status
import fr.valche5.aiusage.domain.WindowDto
import fr.valche5.aiusage.domain.formatClock
import fr.valche5.aiusage.domain.formatDateTime
import fr.valche5.aiusage.domain.formatMoney
import kotlin.math.roundToInt

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DashboardScreen(state: UiState, onRefresh: () -> Unit, onLogout: () -> Unit) {
    Scaffold(
        containerColor = Bg,
        topBar = {
            TopAppBar(
                colors = TopAppBarDefaults.topAppBarColors(containerColor = Bg, titleContentColor = Text),
                title = {
                    Column {
                        Text("ai-usage", fontWeight = FontWeight.Bold)
                        Text(
                            "Dernier refresh : ${formatDateTime(state.reports?.lastRefresh)}",
                            color = Muted,
                            fontSize = 12.sp,
                        )
                    }
                },
                actions = {
                    Text(
                        if (state.live) "live" else "hors ligne",
                        color = if (state.live) Ok else Warn,
                        fontSize = 12.sp,
                        modifier = Modifier.padding(end = 8.dp),
                    )
                    IconButton(onClick = onRefresh, enabled = !state.refreshing) {
                        Icon(Icons.Outlined.Refresh, contentDescription = "Actualiser", tint = Accent)
                    }
                    IconButton(onClick = onLogout) {
                        Icon(Icons.AutoMirrored.Outlined.Logout, contentDescription = "Déconnexion", tint = Muted)
                    }
                },
            )
        },
    ) { padding ->
        PullToRefreshBox(
            isRefreshing = state.refreshing,
            onRefresh = onRefresh,
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            val reports = state.reports?.providers.orEmpty()
            if (reports.isEmpty()) {
                Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                    Text(
                        state.error ?: "Aucun rapport pour l’instant.\nLes connexions se font depuis le dashboard web.",
                        color = Muted,
                    )
                }
            } else {
                LazyColumn(
                    contentPadding = PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    if (!state.error.isNullOrBlank()) {
                        item {
                            Text(
                                state.error,
                                color = Warn,
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .clip(RoundedCornerShape(8.dp))
                                    .background(Card)
                                    .padding(12.dp),
                            )
                        }
                    }
                    items(reports, key = { it.id }) { report ->
                        ReportCard(report)
                    }
                }
            }
        }
    }
}

@Composable
private fun ReportCard(report: ReportDto) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(10.dp))
            .background(Card)
            .padding(17.dp),
    ) {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            Column(Modifier.weight(1f)) {
                Text(report.name, color = Text, fontSize = 18.sp, fontWeight = FontWeight.SemiBold)
                val subtitle = listOfNotNull(
                    report.account.takeIf { it.isNotBlank() },
                    report.plan.takeIf { it.isNotBlank() },
                ).joinToString(" · ")
                if (subtitle.isNotBlank()) {
                    Text(subtitle, color = Muted, fontSize = 13.sp)
                }
            }
            StatusChip(report.status)
        }
        report.windows.forEach { window ->
            Spacer(Modifier.height(14.dp))
            WindowRow(window)
        }
        if (report.reason.isNotBlank()) {
            Text(report.reason, color = Warn, fontSize = 13.sp, modifier = Modifier.padding(top = 12.dp))
        }
        report.warnings.forEach { warning ->
            Text(warning, color = Bad, fontSize = 13.sp, modifier = Modifier.padding(top = 8.dp))
        }
        report.notes.forEach { note ->
            Text(note, color = Muted, fontSize = 13.sp, modifier = Modifier.padding(top = 6.dp))
        }
    }
}

@Composable
private fun StatusChip(status: Status) {
    val (label, color) = when (status) {
        Status.OK -> "ok" to Ok
        Status.STALE -> "stale" to Warn
        Status.ERROR -> "error" to Bad
        Status.UNCONFIGURED -> "n/a" to Muted
    }
    Text(
        label,
        color = color,
        fontSize = 12.sp,
        modifier = Modifier
            .clip(RoundedCornerShape(50))
            .background(SurfaceAlt)
            .padding(horizontal = 8.dp, vertical = 3.dp),
    )
}

@Composable
private fun WindowRow(window: WindowDto) {
    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(window.label, color = Text)
        val value = when {
            window.remainingAmount != null -> formatMoney(window.remainingAmount, window.currency)
            window.unlimited -> "illimité"
            else -> "${window.usedPercent.roundToInt()} %"
        }
        Text(
            value,
            color = if (window.remainingAmount != null) Ok else Text,
            fontWeight = if (window.remainingAmount != null) FontWeight.Bold else FontWeight.Normal,
            fontSize = if (window.remainingAmount != null) 22.sp else 16.sp,
        )
    }
    when {
        window.remainingAmount != null && window.totalAmount != null -> {
            Text("sur ${formatMoney(window.totalAmount, window.currency)} achetés", color = Muted, fontSize = 13.sp)
        }
        !window.unlimited && window.remainingAmount == null -> {
            val fraction = (window.usedPercent / 100.0).coerceIn(0.0, 1.0).toFloat()
            Box(
                Modifier
                    .padding(top = 5.dp)
                    .fillMaxWidth()
                    .height(8.dp)
                    .clip(RoundedCornerShape(99.dp))
                    .background(Line),
            ) {
                Box(
                    Modifier
                        .fillMaxWidth(fraction)
                        .height(8.dp)
                        .clip(RoundedCornerShape(99.dp))
                        .background(Accent),
                )
            }
            if (window.totalCount != null) {
                val used = window.usedCount?.roundToInt() ?: 0
                Text("$used / ${window.totalCount.roundToInt()} crédits utilisés", color = Muted, fontSize = 13.sp)
            }
        }
    }
    if (window.resetsAt != null) {
        Text("reset ${formatClock(window.resetsAt)}", color = Muted, fontSize = 13.sp)
    }
}
