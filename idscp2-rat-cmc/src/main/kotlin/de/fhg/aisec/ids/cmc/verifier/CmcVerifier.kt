/*-
 * ========================LICENSE_START=================================
 * idscp2-rat-cmc
 * %%
 * Copyright (C) 2021 Fraunhofer AISEC
 * %%
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 * 
 *      http://www.apache.org/licenses/LICENSE-2.0
 * 
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 * =========================LICENSE_END==================================
 */
package de.fhg.aisec.ids.cmc.verifier

import com.google.gson.Gson
import de.fhg.aisec.ids.cmc.CmcException
import de.fhg.aisec.ids.cmc.CmcHelper
import de.fhg.aisec.ids.cmc.CmcSocket
import de.fhg.aisec.ids.cmc.messages.AttestationRequest
import de.fhg.aisec.ids.cmc.messages.AttestationResult
import de.fhg.aisec.ids.cmc.messages.VerificationRequest
import de.fhg.aisec.ids.cmc.messages.VerificationResult
import de.fhg.aisec.ids.cmc.toHexString
import de.fhg.aisec.ids.idscp2.idscp_core.drivers.RaVerifierDriver
import de.fhg.aisec.ids.idscp2.idscp_core.fsm.InternalControlMessage
import de.fhg.aisec.ids.idscp2.idscp_core.fsm.fsmListeners.RaVerifierFsmListener
import org.slf4j.LoggerFactory
import java.util.concurrent.BlockingQueue
import java.util.concurrent.LinkedBlockingQueue

/**
 * A CMC RaVerifier driver that verifies the remote peer's identity using CMC
 *
 * @author Leon Beckmann (leon.beckmann@aisec.fraunhofer.de)
 */
class CmcVerifier(fsmListener: RaVerifierFsmListener) : RaVerifierDriver<CmcVerifierConfig>(fsmListener) {
    private val queue: BlockingQueue<ByteArray> = LinkedBlockingQueue()
    private val gson = Gson()
    private lateinit var config: CmcVerifierConfig

    override fun setConfig(config: CmcVerifierConfig) {
        this.config = config
    }

    override fun delegate(message: ByteArray) {
        if (LOG.isTraceEnabled) {
            LOG.trace("Delegated CMC prover message to CMC verifier")
        }
        queue.add(message)
    }

    private fun waitForAttestationReport(): String {
        try {
            return String(queue.take())
        } catch (e: Exception) {
            if (running) {
                fsmListener.onRaVerifierMessage(InternalControlMessage.RA_VERIFIER_FAILED)
            }
            throw CmcException("Interrupted or invalid message", e)
        }
    }

    private fun sendRaResult(result: Boolean) {
        val ratResult = gson.toJson(AttestationResult(true)).toByteArray()
        fsmListener.onRaVerifierMessage(InternalControlMessage.RA_VERIFIER_MSG, ratResult)
        if (result) {
            fsmListener.onRaVerifierMessage(InternalControlMessage.RA_VERIFIER_OK)
        } else {
            fsmListener.onRaVerifierMessage(InternalControlMessage.RA_VERIFIER_FAILED)
        }
    }

    /*
     * ******************* Protocol *******************
     *
     * Verifier:
     * -------------------------
     * Generate NonceV
     * create RaChallenge (NonceV, aType, pcr_mask)
     * -------------------------
     *
     * Prover:
     * -------------------------
     * get RaChallenge (NonceV, aType, pcr_mask)
     * hash = calculateHash(nonceV, certV)
     * req = generate RemoteToTpm (hash, aType, pcr_mask)
     * TpmToRemote = tpmSocket.attestationRequest(req)
     * create TpmResponse from TpmToRemote
     * -------------------------
     *
     * Verifier:
     * -------------------------
     * get TpmResponse
     * hash = calculateHash(nonceV, certV)
     * check signature(response, hash)
     * check golden values from DAT (aType, response)
     * create RaResult
     * -------------------------
     *
     * Prover:
     * -------------------------
     * get TpmResult
     * -------------------------
     *
     */
    override fun run() {
        // CMC Challenge-Response Protocol
        try {
            // create rat challenge with fresh nonce
            if (LOG.isDebugEnabled) {
                LOG.debug("Generate and send challenge to remote prover")
            }
            val nonce = CmcHelper.generateNonce(20).toHexString()
            if (LOG.isDebugEnabled) {
                LOG.debug("Challenge nonce is: $nonce")
            }

            val attestationRequest = AttestationRequest("Attestation Report Request", nonce)
            val ratRequest = gson.toJson(attestationRequest)
            if (LOG.isTraceEnabled) {
                println(ratRequest)
            }
            // send request to prover
            fsmListener.onRaVerifierMessage(InternalControlMessage.RA_VERIFIER_MSG, ratRequest.toByteArray())

            // wait for attestation response
            if (LOG.isDebugEnabled) {
                LOG.debug("Wait for RAT prover message with attestation response")
            }
            // Receive JWS, packed as JSON, from prover
            val attestationReport = waitForAttestationReport()
            if (LOG.isDebugEnabled) {
                LOG.debug("Got challenge response. Start validation...")
            }

            val verificationRequest = VerificationRequest("Verification Request", attestationReport, nonce)
            val verificationRequestJson = gson.toJson(verificationRequest)
            if (LOG.isTraceEnabled) {
                println(verificationRequestJson)
            }
            val resultJson: String
            CmcSocket(config.cmcHost, config.cmcPort).use { cmcSocket ->
                // Pass proof to CMC, get verification result
                resultJson = String(cmcSocket.request(verificationRequestJson.toByteArray()))
            }
            if (LOG.isTraceEnabled) {
                println(resultJson)
            }
            val verificationResult = gson.fromJson(resultJson, VerificationResult::class.java)
            if (verificationResult.raSuccessful) {
                if (LOG.isDebugEnabled) {
                    LOG.debug("CMC verification succeed")
                }
                // Notify prover about success
                sendRaResult(true)
            } else {
                if (LOG.isDebugEnabled) {
                    LOG.debug("CMC verification failed")
                }
                // Notify prover about rejection
                sendRaResult(false)
            }
        } catch (t: Throwable) {
            LOG.error("Error in CMC Verifier", t)
            throw t
        }
    }

    companion object {
        const val ID = "CMC"
        private val LOG = LoggerFactory.getLogger(CmcVerifier::class.java)
    }
}
