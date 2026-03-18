'use strict';

// ==========================================
// 1. MMX Control Client (WebSocket Protocol)
// ==========================================
class MMXControlClient {
    constructor(whepUrl, sessionId, callbacks) {
        this.sessionId = sessionId;
        this.callbacks = callbacks || {};
        this.ws = null;
        this.pingInterval = null;
        this.tracks = [];
        
        this.isIntentionalClose = false; 
        this.reconnectTimer = null;
        this.reconnectDelay = 3000; 

        // 1. 解析 WHEP URL 以提取 host, protocol 和 path
        let pathName = '';
        let wsHost = '';
        let wsProtocol = 'ws:';

        try {
            const urlObj = new URL(whepUrl);
            wsHost = urlObj.host; 
            wsProtocol = urlObj.protocol === 'https:' ? 'wss:' : 'ws:';
            
            // pathname 不包含 query 参数，所以这里是安全的
            const parts = urlObj.pathname.split('/');
            const whepIndex = parts.indexOf('whep');
            if (whepIndex > 0) {
                pathName = parts.slice(1, whepIndex).join('/');
            } else {
                pathName = 'live/stream'; 
            }
        } catch (e) {
            console.error('[WS] URL Parse Error:', e);
        }

        const wsPort = '8810'; 
        const cleanHost = wsHost.split(':')[0];
        
        // 构建 WS URL
        this.wsUrl = `${wsProtocol}//${cleanHost}:${wsPort}/ws/control?session_id=${sessionId}&path=${encodeURIComponent(pathName)}`;

        console.log(`[WS] Path: ${pathName}, URL: ${this.wsUrl}`);
        
        this.connect();
    }

    connect() {
        if (this.reconnectTimer) clearTimeout(this.reconnectTimer);

        console.log(`[WS] Connecting...`);
        this.ws = new WebSocket(this.wsUrl);

        this.ws.onopen = () => {
            console.log('[WS] Connected');
            this.isIntentionalClose = false;
            if (this.callbacks.onConnected) this.callbacks.onConnected();
            this.startHeartbeat(); 
        };

        this.ws.onmessage = (event) => {
            try {
                const msg = JSON.parse(event.data);
                this.handleMessage(msg);
            } catch (e) {
                console.error('[WS] Processing error:', e);
            }
        };

        this.ws.onclose = (e) => {
            console.log(`[WS] Closed (Code: ${e.code}, Reason: ${e.reason})`);
            this.stopHeartbeat();
            
            if (this.callbacks.onDisconnected) this.callbacks.onDisconnected();

            if (!this.isIntentionalClose) {
                console.warn(`[WS] Connection lost unexpectedly. Retrying in ${this.reconnectDelay}ms...`);
                this.reconnectTimer = setTimeout(() => {
                    this.connect();
                }, this.reconnectDelay);
            }
        };

        this.ws.onerror = (err) => {
            console.error('[WS] Connection Error', err);
        };
    }

    handleMessage(msg) {
        if (!msg) return;
        const data = msg.payload || msg.data || {};

        switch (msg.type) {
            case 'TRACKS_INFO':
                let tracks = [];
                let activeId = null;

                if (Array.isArray(data)) {
                    tracks = data;
                } else if (data && Array.isArray(data.tracks)) {
                    tracks = data.tracks;
                    activeId = data.active_track_id;
                } else {
                    console.warn('[WS] Invalid TRACKS_INFO format:', msg);
                    return;
                }

                this.tracks = tracks;
                if (this.callbacks.onTracksInfo) {
                    this.callbacks.onTracksInfo(tracks, activeId);
                }
                break;

            case 'LAYER_SWITCHED':
                console.log(`[WS] Layer switched to Track ID: ${data.current_track_id}`);
                if (this.callbacks.onLayerSwitched) {
                    this.callbacks.onLayerSwitched(data.current_track_id);
                }
                break;

            case 'ERROR':
                const errMsg = data?.message || data?.error || 'Unknown Error';
                const errCode = data?.code || 'N/A';
                console.error(`[WS] Server Error: ${errMsg} (${errCode})`);
                break;

            case 'PONG':
                break;

            default:
                console.warn('[WS] Unknown message type:', msg.type);
        }
    }

    selectLayer(trackId, reason = 'manual') {
        if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
            console.warn('[WS] Cannot switch layer: WebSocket not connected');
            return;
        }
        
        const msg = {
            msg_id: crypto.randomUUID(),
            type: 'SELECT_LAYER',
            timestamp: Math.floor(Date.now() / 1000),
            data: {
                target_track_id: parseInt(trackId),
                reason: reason 
            }
        };
        
        // console.log(`[WS] Sending SELECT_LAYER id=${trackId} reason=${reason}`);
        this.ws.send(JSON.stringify(msg));
    }

    startHeartbeat() {
        this.stopHeartbeat();
        this.pingInterval = setInterval(() => {
            if (this.ws && this.ws.readyState === WebSocket.OPEN) {
                this.ws.send(JSON.stringify({ type: 'PING' }));
            }
        }, 5000);
    }

    stopHeartbeat() {
        if (this.pingInterval) {
            clearInterval(this.pingInterval);
            this.pingInterval = null;
        }
    }

    close() {
        this.isIntentionalClose = true;
        if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
        this.stopHeartbeat();
        if (this.ws) {
            this.ws.close();
            this.ws = null;
        }
    }
}

// ==========================================
// 2. MediaMTX WebRTC Reader (WHEP)
// ==========================================
class MediaMTXWebRTCReader {
  constructor(conf) {
    this.retryPause = 2000;
    this.conf = conf;
    this.state = 'getting_codecs';
    this.restartTimeout = null;
    this.pc = null;
    this.offerData = null;
    this.sessionUrl = null;
    this.queuedCandidates = [];
    this.nonAdvertisedCodecs = [];
    this.#getNonAdvertisedCodecs();
  }

  // [新增] 静态方法：检查设备兼容性
  static async checkSupport() {
    if (!navigator.mediaDevices || !window.RTCPeerConnection) {
        return { supported: false, error: "WebRTC not supported by this browser." };
    }
    try {
        const capabilities = RTCRtpReceiver.getCapabilities('video');
        if (capabilities && capabilities.codecs) {
            const hasH264 = capabilities.codecs.some(c => 
                c.mimeType.toLowerCase() === 'video/h264'
            );
            if (!hasH264) {
                return { supported: false, error: "H.264 codec not supported." };
            }
        }
    } catch (e) {
        console.warn("Codec check failed:", e);
    }
    return { supported: true };
  }

  // [核心修复] 正确解析 Session ID，去除 Query 参数
  get sessionId() {
      if (!this.sessionUrl) return null;
      try {
          // 使用 URL 对象解析，自动处理 Query 参数
          const urlObj = new URL(this.sessionUrl);
          // pathname 不包含 ?txSecret=...
          const pathParts = urlObj.pathname.replace(/\/$/, '').split('/');
          // 获取最后一段作为 UUID
          return pathParts[pathParts.length - 1] || null;
      } catch (e) {
          console.error("Session ID parse error:", e);
          return null;
      }
  }

  close() {
    this.state = 'closed';
    if (this.pc !== null) {
      this.pc.close();
      this.pc = null;
    }
    if (this.restartTimeout !== null) {
      clearTimeout(this.restartTimeout);
      this.restartTimeout = null;
    }
    this.sessionUrl = null;
    this.offerData = null;
    this.queuedCandidates = [];
  }

  static #limitBandwidth(sdp, bitrate) {
    if (!bitrate || parseInt(bitrate) <= 0) return sdp;
    const lines = sdp.split('\r\n');
    let videoIndex = -1;
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].startsWith('m=video')) {
        videoIndex = i;
        break;
      }
    }
    if (videoIndex !== -1) {
      lines.splice(videoIndex + 1, 0, `b=AS:${bitrate}`);
      console.log(`[SDP] Added bandwidth limit b=AS:${bitrate}`);
    }
    return lines.join('\r\n');
  }

  static #linkToIceServers(links) {
    return (links !== null) ? links.split(', ').map((link) => {
      const m = link.match(/^<(.+?)>; rel="ice-server"(; username="(.*?)"; credential="(.*?)"; credential-type="password")?/i);
      const ret = { urls: [m[1]] };
      if (m[3] !== undefined) {
        ret.username = JSON.parse(`"${m[3]}"`);
        ret.credential = JSON.parse(`"${m[4]}"`);
        ret.credentialType = 'password';
      }
      return ret;
    }) : [];
  }

  static #parseOffer(sdp) {
    const ret = { iceUfrag: '', icePwd: '', medias: [] };
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

  static #generateSdpFragment(od, candidates) {
    let frag = `a=ice-ufrag:${od.iceUfrag}\r\n` + `a=ice-pwd:${od.icePwd}\r\n`;
    for (const candidate of candidates) {
      frag += `m=${od.medias[candidate.sdpMLineIndex]}\r\na=mid:${candidate.sdpMLineIndex}\r\na=${candidate.candidate}\r\n`;
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
      this.sessionUrl = null;
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
    this.state = 'running';
    this.#start();
  }

  #start() {
    this.#requestICEServers()
      .then((iceServers) => this.#setupPeerConnection(iceServers))
      .then((offer) => this.#sendOffer(offer))
      .then((answer) => this.#setAnswer(answer))
      .then(() => {
          if (this.conf.onConnected) this.conf.onConnected();
      })
      .catch((err) => {
        console.error(err);
        this.#handleError(err.toString());
      });
  }

  #authHeader() {
    if (this.conf.user) return {'Authorization': `Basic ${btoa(`${this.conf.user}:${this.conf.pass}`)}`};
    if (this.conf.token) return {'Authorization': `Bearer ${this.conf.token}`};
    return {};
  }

  #requestICEServers() {
    return fetch(this.conf.url, {
      method: 'OPTIONS',
      headers: { ...this.#authHeader() },
    }).then((res) => MediaMTXWebRTCReader.#linkToIceServers(res.headers.get('Link')));
  }

  #setupPeerConnection(iceServers) {
    if (this.state !== 'running') throw new Error('closed');

    this.pc = new RTCPeerConnection({
      iceServers,
      sdpSemantics: 'unified-plan',
    });

    const direction = 'recvonly';
    this.pc.addTransceiver('video', { direction });
    this.pc.addTransceiver('audio', { direction });

    this.pc.onicecandidate = (evt) => this.#onLocalCandidate(evt);
    this.pc.onconnectionstatechange = () => this.#onConnectionState();
    this.pc.ontrack = (evt) => this.#onTrack(evt);

    return this.pc.createOffer()
      .then((offer) => {
        let sdp = offer.sdp;
        if (this.conf.maxBitrate) {
            sdp = MediaMTXWebRTCReader.#limitBandwidth(sdp, this.conf.maxBitrate);
        }
        const lines = sdp.split('\r\n').filter(l => 
          !l.toLowerCase().startsWith('a=rid:') && 
          !l.toLowerCase().startsWith('a=simulcast:')
        );
        sdp = lines.join('\r\n');

        this.offerData = MediaMTXWebRTCReader.#parseOffer(sdp);
        const modifiedOffer = new RTCSessionDescription({ type: 'offer', sdp: sdp });
        return this.pc.setLocalDescription(modifiedOffer).then(() => sdp);
      });
  }

  #sendOffer(offer) {
    if (this.state !== 'running') throw new Error('closed');
    return fetch(this.conf.url, {
      method: 'POST',
      headers: { ...this.#authHeader(), 'Content-Type': 'application/sdp' },
      body: offer,
    }).then((res) => {
      if (res.status !== 201) throw new Error(`bad status code ${res.status}`);
      const location = res.headers.get('location');
      if (!location) throw new Error("Missing Location header in response");
      
      this.sessionUrl = new URL(location, this.conf.url).toString();
      console.log(`[WHEP] Session URL: ${this.sessionUrl}`);
      
      if (this.queuedCandidates.length > 0) {
        this.#sendLocalCandidates(this.queuedCandidates);
        this.queuedCandidates = [];
      }
      return res.text();
    });
  }

  #setAnswer(answer) {
    if (this.state !== 'running') throw new Error('closed');
    return this.pc.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: answer }));
  }

  #onLocalCandidate(evt) {
    if (this.state !== 'running') return;
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
      headers: { 'Content-Type': 'application/trickle-ice-sdpfrag', 'If-Match': '*' },
      body: MediaMTXWebRTCReader.#generateSdpFragment(this.offerData, candidates),
    }).catch((err) => {
        console.warn("Candidate patch failed:", err);
    });
  }

  #onConnectionState() {
    if (this.state !== 'running') return;
    if (this.pc.connectionState === 'failed' || this.pc.connectionState === 'closed') {
      this.#handleError('peer connection closed');
    }
  }

  #onTrack(evt) {
    if (this.conf.onTrack !== undefined) this.conf.onTrack(evt);
  }
}
window.MediaMTXWebRTCReader = MediaMTXWebRTCReader;