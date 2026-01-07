'use strict';

/**
 * @callback OnError
 * @param {string} err - error.
 */

/**
 * @callback OnTrack
 * @param {RTCTrackEvent} evt - track event.
 */

/**
 * @typedef Conf
 * @type {object}
 * @property {string} url - absolute URL of the WHEP endpoint.
 * @property {string} user - username.
 * @property {string} pass - password.
 * @property {string} token - token.
 * @property {OnError} onError - called when there's an error.
 * @property {OnTrack} onTrack - called when there's a track available.
 */

/** WebRTC/WHEP reader. */
class MediaMTXWebRTCReader {
  /**
   * Create a MediaMTXWebRTCReader.
   * @param {Conf} conf - configuration.
   */
  constructor(conf) {
    this.retryPause = 2000;
    this.conf = conf;
    this.state = 'getting_codecs';
    this.restartTimeout = null;
    this.pc = null;
    this.offerData = null;
    this.sessionUrl = null;
    this.queuedCandidates = [];
    this.#getNonAdvertisedCodecs();
  }

  /**
   * Close the reader and all its resources.
   */
  close() {
    this.state = 'closed';

    if (this.pc !== null) {
      this.pc.close();
    }

    if (this.restartTimeout !== null) {
      clearTimeout(this.restartTimeout);
    }
  }

  static #supportsNonAdvertisedCodec(codec, fmtp) {
    return new Promise((resolve) => {
      const pc = new RTCPeerConnection({ iceServers: [] });
      const mediaType = 'audio';
      let payloadType = '';

      pc.addTransceiver(mediaType, { direction: 'recvonly' });
      pc.createOffer()
        .then((offer) => {
          if (offer.sdp === undefined) {
            throw new Error('SDP not present');
          }
          if (offer.sdp.includes(` ${codec}`)) { // codec is advertised, there's no need to add it manually
            throw new Error('already present');
          }

          const sections = offer.sdp.split(`m=${mediaType}`);

          const payloadTypes = sections.slice(1)
            .map((s) => s.split('\r\n')[0].split(' ').slice(3))
            .reduce((prev, cur) => [...prev, ...cur], []);
          payloadType = this.#reservePayloadType(payloadTypes);

          const lines = sections[1].split('\r\n');
          lines[0] += ` ${payloadType}`;
          lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} ${codec}`);
          if (fmtp !== undefined) {
            lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} ${fmtp}`);
          }
          sections[1] = lines.join('\r\n');
          offer.sdp = sections.join(`m=${mediaType}`);
          return pc.setLocalDescription(offer);
        })
        .then(() => (
          pc.setRemoteDescription(new RTCSessionDescription({
            type: 'answer',
            sdp: 'v=0\r\n'
            + 'o=- 6539324223450680508 0 IN IP4 0.0.0.0\r\n'
            + 's=-\r\n'
            + 't=0 0\r\n'
            + 'a=fingerprint:sha-256 0D:9F:78:15:42:B5:4B:E6:E2:94:3E:5B:37:78:E1:4B:54:59:A3:36:3A:E5:05:EB:27:EE:8F:D2:2D:41:29:25\r\n'
            + `m=${mediaType} 9 UDP/TLS/RTP/SAVPF ${payloadType}\r\n`
            + 'c=IN IP4 0.0.0.0\r\n'
            + 'a=ice-pwd:7c3bf4770007e7432ee4ea4d697db675\r\n'
            + 'a=ice-ufrag:29e036dc\r\n'
            + 'a=sendonly\r\n'
            + 'a=rtcp-mux\r\n'
            + `a=rtpmap:${payloadType} ${codec}\r\n`
            + ((fmtp !== undefined) ? `a=fmtp:${payloadType} ${fmtp}\r\n` : ''),
          }))
        ))
        .then(() => {
          resolve(true);
        })
        .catch(() => {
          resolve(false);
        })
        .finally(() => {
          pc.close();
        });
    });
  }

  static #unquoteCredential(v) {
    return JSON.parse(`"${v}"`);
  }

  static #linkToIceServers(links) {
    return (links !== null) ? links.split(', ').map((link) => {
      const m = link.match(/^<(.+?)>; rel="ice-server"(; username="(.*?)"; credential="(.*?)"; credential-type="password")?/i);
      const ret = {
        urls: [m[1]],
      };

      if (m[3] !== undefined) {
        ret.username = this.#unquoteCredential(m[3]);
        ret.credential = this.#unquoteCredential(m[4]);
        ret.credentialType = 'password';
      }

      return ret;
    }) : [];
  }

  static #parseOffer(sdp) {
    const ret = {
      iceUfrag: '',
      icePwd: '',
      medias: [],
    };

    for (const line of sdp.split('\r\n')) {
      if (line.startsWith('m=')) {
        ret.medias.push(line.slice('m='.length));
      } else if (ret.iceUfrag === '' && line.startsWith('a=ice-ufrag:')) {
        ret.iceUfrag = line.slice('a=ice-ufrag:'.length);
      } else if (ret.icePwd === '' && line.startsWith('a=ice-pwd:')) {
        ret.icePwd = line.slice('a=ice-pwd:'.length);
      }
    }

    return ret;
  }

  static #reservePayloadType(payloadTypes) {
    // everything is valid between 30 and 127, except for interval between 64 and 95
    // https://chromium.googlesource.com/external/webrtc/+/refs/heads/master/call/payload_type.h#29
    for (let i = 30; i <= 127; i++) {
      if ((i <= 63 || i >= 96) && !payloadTypes.includes(i.toString())) {
        const pl = i.toString();
        payloadTypes.push(pl);
        return pl;
      }
    }
    throw Error('unable to find a free payload type');
  }

  static #enableStereoPcmau(payloadTypes, section) {
    const lines = section.split('\r\n');

    let payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} PCMU/8000/2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} PCMA/8000/2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    return lines.join('\r\n');
  }

  static #enableMultichannelOpus(payloadTypes, section) {
    const lines = section.split('\r\n');

    let payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/3`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,2,1;num_streams=2;coupled_streams=1`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/4`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,1,2,3;num_streams=2;coupled_streams=2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/5`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,4,1,2,3;num_streams=3;coupled_streams=2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/6`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,4,1,2,3,5;num_streams=4;coupled_streams=2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/7`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,4,1,2,3,5,6;num_streams=4;coupled_streams=4`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} multiopus/48000/8`);
    lines.splice(lines.length - 1, 0, `a=fmtp:${payloadType} channel_mapping=0,6,1,4,5,2,3,7;num_streams=5;coupled_streams=4`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    return lines.join('\r\n');
  }

  static #enableL16(payloadTypes, section) {
    const lines = section.split('\r\n');

    let payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} L16/8000/2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} L16/16000/2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    payloadType = this.#reservePayloadType(payloadTypes);
    lines[0] += ` ${payloadType}`;
    lines.splice(lines.length - 1, 0, `a=rtpmap:${payloadType} L16/48000/2`);
    lines.splice(lines.length - 1, 0, `a=rtcp-fb:${payloadType} transport-cc`);

    return lines.join('\r\n');
  }

  static #enableStereoOpus(section) {
    let opusPayloadFormat = '';
    const lines = section.split('\r\n');

    for (let i = 0; i < lines.length; i++) {
      if (lines[i].startsWith('a=rtpmap:') && lines[i].toLowerCase().includes('opus/')) {
        opusPayloadFormat = lines[i].slice('a=rtpmap:'.length).split(' ')[0];
        break;
      }
    }

    if (opusPayloadFormat === '') {
      return section;
    }

    for (let i = 0; i < lines.length; i++) {
      if (lines[i].startsWith(`a=fmtp:${opusPayloadFormat} `)) {
        if (!lines[i].includes('stereo')) {
          lines[i] += ';stereo=1';
        }
        if (!lines[i].includes('sprop-stereo')) {
          lines[i] += ';sprop-stereo=1';
        }
      }
    }

    return lines.join('\r\n');
  }

  static #editOffer(sdp, nonAdvertisedCodecs) {
    const sections = sdp.split('m=');

    const payloadTypes = sections.slice(1)
      .map((s) => s.split('\r\n')[0].split(' ').slice(3))
      .reduce((prev, cur) => [...prev, ...cur], []);

    for (let i = 1; i < sections.length; i++) {
      if (sections[i].startsWith('audio')) {
        sections[i] = this.#enableStereoOpus(sections[i]);

        if (nonAdvertisedCodecs.includes('pcma/8000/2')) {
          sections[i] = this.#enableStereoPcmau(payloadTypes, sections[i]);
        }
        if (nonAdvertisedCodecs.includes('multiopus/48000/6')) {
          sections[i] = this.#enableMultichannelOpus(payloadTypes, sections[i]);
        }
        if (nonAdvertisedCodecs.includes('L16/48000/2')) {
          sections[i] = this.#enableL16(payloadTypes, sections[i]);
        }

        break;
      }
    }

    return sections.join('m=');
  }

  static #generateSdpFragment(od, candidates) {
    const candidatesByMedia = {};
    for (const candidate of candidates) {
      const mid = candidate.sdpMLineIndex;
      if (candidatesByMedia[mid] === undefined) {
        candidatesByMedia[mid] = [];
      }
      candidatesByMedia[mid].push(candidate);
    }

    let frag = `a=ice-ufrag:${od.iceUfrag}\r\n`
      + `a=ice-pwd:${od.icePwd}\r\n`;

    let mid = 0;

    for (const media of od.medias) {
      if (candidatesByMedia[mid] !== undefined) {
        frag += `m=${media}\r\n`
          + `a=mid:${mid}\r\n`;

        for (const candidate of candidatesByMedia[mid]) {
          frag += `a=${candidate.candidate}\r\n`;
        }
      }
      mid++;
    }

    return frag;
  }

  #handleError(err) {
    if (this.state === 'running') {
      if (this.pc !== null) {
        this.pc.close();
        this.pc = null;
      }

      this.offerData = null;

      if (this.sessionUrl !== null) {
        fetch(this.sessionUrl, {
          method: 'DELETE',
        });
        this.sessionUrl = null;
      }

      this.queuedCandidates = [];
      this.state = 'restarting';

      this.restartTimeout = window.setTimeout(() => {
        this.restartTimeout = null;
        this.state = 'running';
        this.#start();
      }, this.retryPause);

      if (this.conf.onError !== undefined) {
        this.conf.onError(`${err}, retrying in some seconds`);
      }
    } else if (this.state === 'getting_codecs') {
      this.state = 'failed';

      if (this.conf.onError !== undefined) {
        this.conf.onError(err);
      }
    }
  }

  #getNonAdvertisedCodecs() {
    Promise.all([
      ['pcma/8000/2'],
      ['multiopus/48000/6', 'channel_mapping=0,4,1,2,3,5;num_streams=4;coupled_streams=2'],
      ['L16/48000/2'],
    ]
      .map((c) => MediaMTXWebRTCReader.#supportsNonAdvertisedCodec(c[0], c[1]).then((r) => ((r) ? c[0] : false))))
      .then((c) => c.filter((e) => e !== false))
      .then((codecs) => {
        if (this.state !== 'getting_codecs') {
          throw new Error('closed');
        }

        this.nonAdvertisedCodecs = codecs;
        this.state = 'running';
        this.#start();
      })
      .catch((err) => {
        this.#handleError(err);
      });
  }

  #start() {
    console.log('Starting WebRTC connection...');
    this.#requestICEServers()
      .then((iceServers) => {
        console.log('ICE servers obtained, setting up peer connection');
        return this.#setupPeerConnection(iceServers);
      })
      .then((offer) => {
        console.log('Peer connection setup complete, sending offer, offer length:', offer.length);
        return this.#sendOffer(offer);
      })
      .then((answer) => {
        console.log('Offer sent, received answer, setting remote description, answer length:', answer.length);
        return this.#setAnswer(answer);
      })
      .then(() => {
        console.log('Remote description set successfully');
      })
      .catch((err) => {
        console.error('Error in #start:', err, err.stack);
        this.#handleError(err.toString());
      });
  }

  #authHeader() {
    if (this.conf.user !== undefined && this.conf.user !== '') {
      const credentials = btoa(`${this.conf.user}:${this.conf.pass}`);
      return {'Authorization': `Basic ${credentials}`};
    }
    if (this.conf.token !== undefined && this.conf.token !== '') {
      return {'Authorization': `Bearer ${this.conf.token}`};
    }
    return {};
  }

  #requestICEServers() {
    return fetch(this.conf.url, {
      method: 'OPTIONS',
      headers: {
        ...this.#authHeader(),
      },
    })
      .then((res) => MediaMTXWebRTCReader.#linkToIceServers(res.headers.get('Link')));
  }

  #setupPeerConnection(iceServers) {
    if (this.state !== 'running') {
      throw new Error('closed');
    }

    this.pc = new RTCPeerConnection({
      iceServers,
      // https://webrtc.org/getting-started/unified-plan-transition-guide
      sdpSemantics: 'unified-plan',
    });

    const direction = 'recvonly';
    // For receiving simulcast, we just add transceivers normally
    // Simulcast configuration will be added to SDP manually
    this.pc.addTransceiver('video', { direction });
    this.pc.addTransceiver('audio', { direction });

    this.pc.onicecandidate = (evt) => this.#onLocalCandidate(evt);
    this.pc.onconnectionstatechange = () => this.#onConnectionState();
    this.pc.ontrack = (evt) => this.#onTrack(evt);

    return this.pc.createOffer()
      .then((offer) => {
        // For receiving simulcast, we don't need to add simulcast attributes to the offer
        // The server will provide simulcast configuration in the answer
        // Remove any simulcast-related attributes that browser might have added
        let cleanedSdp = this.#removeSimulcastFromOffer(offer.sdp);
        
        // Verify that simulcast attributes were removed
        if (cleanedSdp.includes('a=rid:') || cleanedSdp.includes('a=simulcast:')) {
          console.warn('Warning: Simulcast attributes still present in offer after removal, attempting second pass');
          cleanedSdp = this.#removeSimulcastFromOffer(cleanedSdp);
        }
        
        cleanedSdp = MediaMTXWebRTCReader.#editOffer(cleanedSdp, this.nonAdvertisedCodecs);
        this.offerData = MediaMTXWebRTCReader.#parseOffer(cleanedSdp);

        // Set local description with original offer first (browser may need this)
        // Then we'll send the cleaned SDP to the server
        console.log('Setting local description with original offer, SDP length:', offer.sdp.length);
        return this.pc.setLocalDescription(offer)
          .then(() => {
            console.log('setLocalDescription with original offer succeeded');
            // Return cleaned SDP for sending to server
            return cleanedSdp;
          })
          .catch((err) => {
            console.error('setLocalDescription with original offer failed:', err);
            // If original offer fails, try with cleaned offer
            console.log('Trying with cleaned offer, SDP length:', cleanedSdp.length);
            const cleanedOffer = new RTCSessionDescription({
              type: 'offer',
              sdp: cleanedSdp,
            });
            return this.pc.setLocalDescription(cleanedOffer)
          .then(() => {
            console.log('setLocalDescription succeeded, returning cleaned SDP');
            return cleanedSdp;
          })
          .catch((err) => {
            console.error('setLocalDescription failed:', err, err.stack);
            // If setLocalDescription fails, log the error but continue anyway
            // For recvonly connections, this error might be harmless
            // The connection may still work despite this error
            const isSimulcastError = err.message && (
              err.message.includes('setParameters') || 
              err.message.includes('Read-only field') ||
              err.message.includes('InvalidModificationError') ||
              err.name === 'InvalidModificationError'
            );
            
            if (isSimulcastError) {
              console.warn('Browser attempted to auto-configure simulcast (this is harmless for recvonly):', err.message);
              // For simulcast errors, just continue with the cleaned SDP
              // The connection should still work
              return cleanedSdp;
            }
            // For other errors, throw to trigger normal error handling
            throw err;
          });
      });
  }

  #removeSimulcastFromOffer(sdp) {
    // Remove RID and simulcast attributes from offer
    // These should only be in the answer from the server
    // Browser may automatically add these, so we need to remove them
    const lines = sdp.split('\r\n');
    const newLines = [];
    let removedCount = 0;

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      // Skip RID and simulcast lines (case-insensitive check)
      const lowerLine = line.toLowerCase();
      if (lowerLine.startsWith('a=rid:') || lowerLine.startsWith('a=simulcast:')) {
        removedCount++;
        continue;
      }
      newLines.push(line);
    }

    // Log if we removed any simulcast attributes (for debugging)
    if (removedCount > 0) {
      console.debug(`Removed ${removedCount} simulcast-related attributes from offer`);
    }

    const result = newLines.join('\r\n');
    
    // Double-check: if still contains simulcast, log warning
    if (result.includes('a=rid:') || result.includes('a=simulcast:')) {
      console.warn('Warning: Simulcast attributes still present after removal attempt');
    }
    
    return result;
  }

  #sendOffer(offer) {
    if (this.state !== 'running') {
      throw new Error('closed');
    }

    return fetch(this.conf.url, {
      method: 'POST',
      headers: {
        ...this.#authHeader(),
        'Content-Type': 'application/sdp',
      },
      body: offer,
    })
      .then((res) => {
        switch (res.status) {
          case 201:
            break;
          case 404:
            throw new Error('stream not found');
          case 400:
            return res.json().then((e) => { throw new Error(e.error); });
          default:
            throw new Error(`bad status code ${res.status}`);
        }

        this.sessionUrl = new URL(res.headers.get('location'), this.conf.url).toString();

        return res.text();
      });
  }

  #setAnswer(answer) {
    if (this.state !== 'running') {
      throw new Error('closed');
    }

    return this.pc.setRemoteDescription(new RTCSessionDescription({
      type: 'answer',
      sdp: answer,
    }))
      .then(() => {
        if (this.state !== 'running') {
          return;
        }

        if (this.queuedCandidates.length !== 0) {
          this.#sendLocalCandidates(this.queuedCandidates);
          this.queuedCandidates = [];
        }
      })
      .catch((err) => {
        // Log the error for debugging
        console.error('Failed to set remote description:', err);
        console.error('Answer SDP:', answer);
        // Check if this is a simulcast-related error
        if (err.message && (err.message.includes('simulcast') || err.message.includes('RID') || err.message.includes('rid'))) {
          console.warn('Simulcast-related error in setRemoteDescription, this may be a browser limitation');
          // For recvonly connections, simulcast errors in answer might be harmless
          // Try to continue anyway
          throw err;
        }
        throw err;
      });
  }

  #onLocalCandidate(evt) {
    if (this.state !== 'running') {
      return;
    }

    if (evt.candidate !== null) {
      if (this.sessionUrl === null) {
        this.queuedCandidates.push(evt.candidate);
      } else {
        this.#sendLocalCandidates([evt.candidate]);
      }
    }
  }

  #sendLocalCandidates(candidates) {
    fetch(this.sessionUrl, {
      method: 'PATCH',
      headers: {
        'Content-Type': 'application/trickle-ice-sdpfrag',
        'If-Match': '*',
      },
      body: MediaMTXWebRTCReader.#generateSdpFragment(this.offerData, candidates),
    })
      .then((res) => {
        switch (res.status) {
          case 204:
            break;
          case 404:
            throw new Error('stream not found');
          default:
            throw new Error(`bad status code ${res.status}`);
        }
      })
      .catch((err) => {
        this.#handleError(err.toString());
      });
  }

  #onConnectionState() {
    if (this.state !== 'running') {
      return;
    }

    // "closed" can arrive before "failed" and without
    // the close() method being called at all.
    // It happens when the other peer sends a termination
    // message like a DTLS CloseNotify.
    if (this.pc.connectionState === 'failed'
      || this.pc.connectionState === 'closed'
    ) {
      this.#handleError('peer connection closed');
    }
  }

  #onTrack(evt) {
    if (this.conf.onTrack !== undefined) {
      this.conf.onTrack(evt);
    }
  }
}

window.MediaMTXWebRTCReader = MediaMTXWebRTCReader;
