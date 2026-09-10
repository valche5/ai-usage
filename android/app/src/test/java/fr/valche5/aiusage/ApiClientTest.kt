package fr.valche5.aiusage

import fr.valche5.aiusage.domain.Status
import fr.valche5.aiusage.net.ApiClient
import fr.valche5.aiusage.net.ApiException
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

class ApiClientTest {
    private val reportsJson = """
        {
          "schema_version": 1,
          "generated_at": "2026-09-11T00:00:00Z",
          "last_refresh": "2026-09-11T00:01:00Z",
          "providers": [
            {
              "id": "openrouter",
              "name": "OpenRouter",
              "status": "ok",
              "source": "live",
              "windows": [
                {
                  "key": "solde",
                  "label": "crédit restant",
                  "used_percent": 32.0,
                  "remaining_amount": 17.34,
                  "total_amount": 25.50,
                  "currency": "USD"
                }
              ]
            }
          ]
        }
    """.trimIndent()

    @Test
    fun bearerFetchesReports() {
        val server = MockWebServer()
        server.enqueue(MockResponse().setBody(reportsJson))
        server.start()
        try {
            val client = ApiClient()
            client.baseUrl = server.url("/").toString().trimEnd('/')
            val reports = client.connect("secret-token")
            assertEquals(1, reports.providers.size)
            val report = reports.providers[0]
            assertEquals("openrouter", report.id)
            assertEquals(Status.OK, report.status)
            assertEquals(17.34, report.windows[0].remainingAmount)
            val request = server.takeRequest()
            assertEquals("/api/reports", request.path)
            assertEquals("Bearer secret-token", request.getHeader("Authorization"))
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun bearerRejected() {
        val server = MockWebServer()
        server.enqueue(MockResponse().setResponseCode(401).setBody("authentification requise"))
        server.start()
        try {
            val client = ApiClient()
            client.baseUrl = server.url("/").toString().trimEnd('/')
            try {
                client.connect("nope")
                fail("expected ApiException")
            } catch (e: ApiException) {
                assertEquals(401, e.status)
                assertTrue(e.message!!.contains("jeton"))
            }
        } finally {
            server.shutdown()
        }
    }

    @Test
    fun refreshSendsBearerWithoutCsrf() {
        val server = MockWebServer()
        server.enqueue(MockResponse().setResponseCode(303).addHeader("Location", "/"))
        server.start()
        try {
            val client = ApiClient()
            client.baseUrl = server.url("/").toString().trimEnd('/')
            client.token = "secret-token"
            client.requestRefresh()
            val request = server.takeRequest()
            assertEquals("/refresh", request.path)
            assertEquals("POST", request.method)
            assertEquals("Bearer secret-token", request.getHeader("Authorization"))
            val body = request.body.readUtf8()
            assertTrue(!body.contains("csrf"))
        } finally {
            server.shutdown()
        }
    }
}
