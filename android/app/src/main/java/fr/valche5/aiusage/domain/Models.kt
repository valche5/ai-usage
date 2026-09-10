package fr.valche5.aiusage.domain

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

@Serializable
enum class Status {
    @SerialName("ok") OK,
    @SerialName("stale") STALE,
    @SerialName("unconfigured") UNCONFIGURED,
    @SerialName("error") ERROR,
}

@Serializable
enum class Source {
    @SerialName("live") LIVE,
    @SerialName("cache") CACHE,
    @SerialName("local") LOCAL,
    @SerialName("") NONE,
}

@Serializable
data class WindowDto(
    val key: String = "",
    val label: String = "",
    @SerialName("used_percent") val usedPercent: Double = 0.0,
    @SerialName("resets_at") val resetsAt: String? = null,
    @SerialName("window_minutes") val windowMinutes: Int = 0,
    val unlimited: Boolean = false,
    @SerialName("remaining_amount") val remainingAmount: Double? = null,
    @SerialName("total_amount") val totalAmount: Double? = null,
    val currency: String = "",
    @SerialName("used_count") val usedCount: Double? = null,
    @SerialName("total_count") val totalCount: Double? = null,
)

@Serializable
data class ReportDto(
    val id: String,
    val name: String,
    val plan: String = "",
    val status: Status = Status.UNCONFIGURED,
    val source: Source = Source.NONE,
    val windows: List<WindowDto> = emptyList(),
    val notes: List<String> = emptyList(),
    @SerialName("fetched_at") val fetchedAt: String? = null,
    val reason: String = "",
    val account: String = "",
    @SerialName("account_id") val accountId: String = "",
    val warnings: List<String> = emptyList(),
)

@Serializable
data class ReportsPayload(
    val type: String = "",
    @SerialName("schema_version") val schemaVersion: Int = 1,
    @SerialName("generated_at") val generatedAt: String? = null,
    @SerialName("last_refresh") val lastRefresh: String? = null,
    val providers: List<ReportDto> = emptyList(),
)

private val clockFmt: DateTimeFormatter =
    DateTimeFormatter.ofPattern("dd/MM HH:mm", Locale.FRANCE)

private val dateTimeFmt: DateTimeFormatter =
    DateTimeFormatter.ofPattern("dd/MM/yyyy HH:mm:ss", Locale.FRANCE)

fun parseInstant(value: String?): Instant? {
    if (value.isNullOrBlank()) return null
    return runCatching { Instant.parse(value) }.getOrNull()
}

fun formatClock(value: String?): String {
    val instant = parseInstant(value) ?: return "—"
    return clockFmt.format(instant.atZone(ZoneId.systemDefault()))
}

fun formatDateTime(value: String?): String {
    val instant = parseInstant(value) ?: return "en attente"
    return dateTimeFmt.format(instant.atZone(ZoneId.systemDefault()))
}

fun formatMoney(value: Double, currency: String): String =
    if (currency.equals("USD", ignoreCase = true)) {
        "$" + "%.2f".format(Locale.US, value)
    } else {
        "%.2f %s".format(Locale.FRANCE, value, currency)
    }
