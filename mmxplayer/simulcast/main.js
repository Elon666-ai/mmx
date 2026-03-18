// main.js - Fixed version with video resume handling

const urlInput = document.getElementById('urlInput');
const video = document.getElementById('video');
const statsContainer = document.getElementById('stats');
const layerSelect = document.getElementById('layerSelect');
const wsStatusDot = document.getElementById('wsStatus');

let reader = null;
let controlClient = null;
let statsInterval = null;
let lastStats = { videoBytes: 0, audioBytes: 0, timestamp: 0 };
let previousTrackType = null; // Track if we were in audio-only mode

// 实例化 ABR 引擎
const abrEngine = new ABREngine({
    onSwitchLayer: (trackId, reason) => {
        if (controlClient) {
            console.log(`[Main] ABR Triggered Switch: ${trackId} (${reason})`);
            controlClient.selectLayer(trackId, reason);
        }
    }
});

// [修复] 视频分辨率监控：增加判重逻辑
let lastWidth = 0;
let lastHeight = 0;
let loadStartTime = 0;
let isFirstFrameLogged = false;

video.addEventListener('resize', () => {
    const w = video.videoWidth;
    const h = video.videoHeight;
    
    // 如果尺寸没变，忽略（过滤掉 metadata 加载或浏览器内部重绘触发的 resize）
    if (w === lastWidth && h === lastHeight) return;
    
    lastWidth = w;
    lastHeight = h;

    const msg = `[Video] Resolution Changed: ${w}x${h}`;
    console.log(`%c${msg}`, 'background: #222; color: #bada55; font-size: 16px; padding: 4px; border-radius: 4px;');
    // showToast(msg);
});

video.addEventListener('loadedmetadata', () => {
    console.log(`[Video] Metadata loaded. Initial size: ${video.videoWidth}x${video.videoHeight}`);
});

// ✅ NEW: Monitor video state for debugging
video.addEventListener('waiting', () => {
    console.log('[Video] State: WAITING (buffering)');
});

video.addEventListener('playing', () => {
    if (loadStartTime > 0 && !isFirstFrameLogged) {
        const loadTime = Date.now() - loadStartTime;
        console.log(`[Stats] Video Loaded in: ${loadTime} ms`);
        isFirstFrameLogged = true;
    }
});

video.addEventListener('pause', () => {
    console.log('[Video] State: PAUSED');
});

function showToast(text) {
    const toast = document.createElement('div');
    toast.innerText = text;
    toast.style.cssText = `
        position: absolute; top: 20px; left: 50%; transform: translateX(-50%);
        background: rgba(40, 167, 69, 0.9); color: white; padding: 10px 20px;
        border-radius: 20px; font-weight: bold; z-index: 1000; transition: opacity 0.5s;
    `;
    document.body.appendChild(toast);
    setTimeout(() => {
        toast.style.opacity = '0';
        setTimeout(() => toast.remove(), 500);
    }, 3000);
}

document.getElementById('startBtn').addEventListener('click', startStream);
document.getElementById('exitBtn').addEventListener('click', stopStream);
document.getElementById('pauseBtn').onclick = togglePauseStream;

layerSelect.addEventListener('change', (e) => {
    const val = e.target.value;
    if (val === "auto") {
        abrEngine.setAutoMode(true);
        console.log(`[UI] Switched to AUTO mode`);
    } else {
        abrEngine.setAutoMode(false);
        const trackId = parseInt(val);
        if (!isNaN(trackId) && controlClient) {
            console.log(`[UI] Manual select track: ${trackId}`);
            // 通知引擎手动切换了，更新其内部状态
            abrEngine.notifyManualSwitch(trackId);
            controlClient.selectLayer(trackId, 'manual_user');
        }
    }
});

async function startStream() {
    stopStream(); 

    const url = urlInput.value.trim();
    if (!url) return alert('Please enter a WHEP URL');

    loadStartTime = Date.now();
    isFirstFrameLogged = false;
    const check = await MediaMTXWebRTCReader.checkSupport();
    if (!check.supported) {
        alert("Playback Not Supported:\n" + check.error);
        statsContainer.innerHTML = `<div style="color: red; text-align: center;">${check.error}</div>`;
        return;
    }

    statsContainer.innerHTML = '<div style="color: #00bcd4; text-align: center;">Connecting WHEP...</div>';
    previousTrackType = null;

    reader = new MediaMTXWebRTCReader({
        // ... (保持不变)
        url: url,
        maxBitrate: 2500, 
        onTrack: (evt) => {
            if (evt.track.kind === 'video' || evt.track.kind === 'audio') {
                if (video.srcObject !== evt.streams[0]) {
                    video.srcObject = evt.streams[0];
                }
            }
        },
        onError: (err) => {
            console.error("Reader Error:", err);
            // 这里会显示 "Retrying... (1/5)" 或 "Playback failed"
            statsContainer.innerHTML = `<div style="color: red; text-align: center;">${err}</div>`;
        },
        onConnected: () => {
            const sessionId = reader.sessionId;
            console.log(`[Glue] WHEP Connected. SessionID: ${sessionId}`);
            if (sessionId) {
                initControlClient(url, sessionId);
            }
        }
    });

    lastStats.timestamp = Date.now();
    statsInterval = setInterval(updateStats, 1000);
}

function initControlClient(whepUrl, sessionId) {
    controlClient = new MMXControlClient(whepUrl, sessionId, {
        onConnected: () => {
            wsStatusDot.classList.remove('ws-disconnected');
            wsStatusDot.classList.add('ws-connected');
            layerSelect.disabled = false;
            layerSelect.innerHTML = '<option value="-1">Loading...</option>';
        },
        onDisconnected: () => {
            wsStatusDot.classList.remove('ws-connected');
            wsStatusDot.classList.add('ws-disconnected');
            layerSelect.disabled = true;
        },
        onTracksInfo: (tracks, activeId) => {
            console.log("[UI] Received Tracks Info:", tracks);
            
            // [关键修复] 获取当前视频实际宽度，传给 ABR 引擎用于探测真实初始状态
            const currentVideoWidth = video.videoWidth || 0;
            abrEngine.setTracks(tracks, activeId, currentVideoWidth);
            
            if (!layerSelect.disabled){
                updateLayerSelectUI(tracks, activeId);
            }
        },
        onLayerSwitched: (id) => {
            console.log("[UI] Received Track switch:", id);

            const currentTrack = abrEngine.trackRegistry[id];
            const wasAudioOnly = previousTrackType === 'audio';
            const isNowVideo = currentTrack && currentTrack.type === 'video';
            
            if (wasAudioOnly && isNowVideo) {
                console.log('[Main] Resuming from audio-only to video - forcing video play');
                handleVideoResume();
            }
            
            previousTrackType = currentTrack ? currentTrack.type : null;
            abrEngine.notifyLayerSwitched(id);
            
            if (!abrEngine.isAutoMode && !layerSelect.disabled) {
                layerSelect.value = id;
            }
        }
    });
}

// ✅ NEW: Handle video resume after audio-only mode
function handleVideoResume() {
    if (!video || !video.srcObject) {
        console.warn('[Main] Cannot resume video - no video element or stream');
        return;
    }
    
    // Strategy 1: Ensure video is not paused
    if (video.paused) {
        console.log('[Main] Video is paused, attempting to play...');
        video.play().catch(err => {
            console.warn('[Main] Auto-play failed:', err);
        });
    }
    
    // Strategy 2: Force a small seek to trigger rendering
    // This helps in cases where the video element is "stuck"
    setTimeout(() => {
        if (video.readyState >= 2) { // HAVE_CURRENT_DATA or better
            const currentTime = video.currentTime;
            if (currentTime > 0.01) {
                video.currentTime = currentTime - 0.01;
                console.log('[Main] Applied micro-seek to trigger video rendering');
            }
        }
    }, 100);
    
    // Strategy 3: Force video element refresh
    // Some browsers need this to properly resume video rendering
    setTimeout(() => {
        if (video.paused && video.srcObject) {
            console.log('[Main] Video still paused after 500ms, forcing play');
            video.play().catch(err => {
                console.warn('[Main] Delayed auto-play failed:', err);
            });
        }
    }, 500);
}

function updateLayerSelectUI(tracks, activeId) {
    layerSelect.innerHTML = '';
    
    const autoOption = document.createElement('option');
    autoOption.value = 'auto';
    autoOption.text = 'Auto (ABR)';
    layerSelect.appendChild(autoOption);

    const videos = tracks.filter(t => t.type === 'video');
    // UI显示：码率从高到低
    videos.sort((a, b) => (b.bitrate || 0) - (a.bitrate || 0));

    videos.forEach(t => {
        const option = document.createElement('option');
        option.value = t.id;
        let label = t.label || `Video ${t.id}`;
        if (t.height) label += ` (${t.height}p)`;
        if (t.bitrate) label += ` ${(t.bitrate/1000).toFixed(0)}k`;
        option.text = label;
        layerSelect.appendChild(option);
    });

    const audios = tracks.filter(t => t.type === 'audio');
    if (audios.length > 0) {
        const audioT = audios[0];
        const option = document.createElement('option');
        option.value = audioT.id;
        option.text = `Audio Only (${(audioT.bitrate/1000).toFixed(0)}k)`;
        option.style.fontWeight = 'bold';
        option.style.color = '#ff9800'; 
        layerSelect.appendChild(option);
    }

    if (activeId !== undefined && activeId !== null) {
        if (!abrEngine.isAutoMode) {
            layerSelect.value = activeId;
        } else {
            layerSelect.value = 'auto';
        }
    }
}

function stopStream() {
    if (controlClient) {
        controlClient.close();
        controlClient = null;
    }
    if (reader) {
        reader.close();
        reader = null;
    }
    if (statsInterval) {
        clearInterval(statsInterval);
        statsInterval = null;
    }
    video.srcObject = null;
    statsContainer.innerHTML = '<div style="color: #888; text-align: center;">Stopped</div>';
    
    wsStatusDot.classList.remove('ws-connected');
    wsStatusDot.classList.remove('ws-disconnected');
    layerSelect.disabled = true;
    layerSelect.innerHTML = '<option value="-1">Auto</option>';
    
    abrEngine.setAutoMode(true);
    previousTrackType = null;
    loadStartTime = 0;
    isFirstFrameLogged = false;    
}

async function updateStats() {
    if (!reader || !reader.pc) return;
    const pc = reader.pc;
    if (pc.connectionState !== 'connected' && pc.connectionState !== 'checking') return;

    try {
        const stats = await pc.getStats();
        const now = Date.now();
        const deltaTime = (now - lastStats.timestamp) / 1000;
        if (deltaTime <= 0) return;

        let videoStats = null;
        let audioStats = null;
        let networkStats = null;
        // [新增] 用于查找 codec 名称
        const codecs = new Map(); 

        stats.forEach(report => {
            if (report.type === 'inbound-rtp' && report.kind === 'video') videoStats = report;
            if (report.type === 'inbound-rtp' && report.kind === 'audio') audioStats = report;
            if (report.type === 'candidate-pair' && report.state === 'succeeded') networkStats = report;
            // [新增] 收集 codec 信息
            if (report.type === 'codec') {
                codecs.set(report.id, report.mimeType); // e.g. "video/H264"
            }
        });

        // --- 计算实时指标 ---
        let videoKbps = 0;
        let audioKbps = 0;
        let fps = 0;
        let currentPacketLoss = 0;

        if (videoStats) {
            videoKbps = ((videoStats.bytesReceived - lastStats.videoBytes) * 8 / deltaTime / 1000);
            fps = videoStats.framesPerSecond || 0;
            const vLoss = (videoStats.packetsLost || 0) - (lastStats.videoPacketsLost || 0);
            if (vLoss > 0) currentPacketLoss += vLoss;
            lastStats.videoBytes = videoStats.bytesReceived;
            lastStats.videoPacketsLost = videoStats.packetsLost || 0;
        }

        if (audioStats) {
            audioKbps = ((audioStats.bytesReceived - lastStats.audioBytes) * 8 / deltaTime / 1000);
            const aLoss = (audioStats.packetsLost || 0) - (lastStats.audioPacketsLost || 0);
            if (aLoss > 0) currentPacketLoss += aLoss;
            lastStats.audioBytes = audioStats.bytesReceived;
            lastStats.audioPacketsLost = audioStats.packetsLost || 0;
        }

        lastStats.timestamp = now;

        // --- 调用 ABR 引擎 ---
        if (abrEngine) {
            abrEngine.update(videoKbps, audioKbps, fps, currentPacketLoss);
        }

        // --- 渲染 UI ---
        let html = '';

        if (videoStats) {
            const displayW = video.videoWidth || videoStats.frameWidth || 0;
            const displayH = video.videoHeight || videoStats.frameHeight || 0;
            
            // [新增] 获取 Video Codec 名称
            let vCodec = 'N/A';
            if (videoStats.codecId && codecs.has(videoStats.codecId)) {
                // mimeType 格式通常为 "video/H264"，我们只取后半部分
                vCodec = codecs.get(videoStats.codecId).split('/')[1] || 'Unknown';
            }

            html += renderStatGroup('Video', {
                'Codec': vCodec, // [显示]
                'Resolution': `${displayW}x${displayH}`,
                'Recv Bitrate': `${videoKbps.toFixed(0)} kbps`,
                'FPS': fps.toFixed(1),
                'Packet Loss': `${videoStats.packetsLost} pkts`
            });
        }

        if (audioStats) {
            // [新增] 获取 Audio Codec 名称
            let aCodec = 'N/A';
            if (audioStats.codecId && codecs.has(audioStats.codecId)) {
                aCodec = codecs.get(audioStats.codecId).split('/')[1] || 'Unknown';
            }

            html += renderStatGroup('Audio', {
                'Codec': aCodec, // [显示]
                'Recv Bitrate': `${audioKbps.toFixed(0)} kbps`,
                'Packet Loss': `${audioStats.packetsLost} pkts`,
                'Jitter': `${(audioStats.jitter * 1000).toFixed(1)} ms`
            });
        }

        if (networkStats) {
            let bw = 'N/A';
            if (networkStats.availableIncomingBitrate) {
                bw = `${(networkStats.availableIncomingBitrate / 1000).toFixed(0)} kbps`;
            }
            
            let abrStatus = (abrEngine && abrEngine.isAutoMode) ? 'Auto' : 'Manual';
            if (abrEngine && abrEngine.abrCooldown > 0) abrStatus += ` (Cool ${abrEngine.abrCooldown})`;

            html += renderStatGroup('Network', {
                'RTT': `${(networkStats.currentRoundTripTime * 1000).toFixed(1)} ms`,
                'Est. Bandwidth': bw,
                'ABR State': abrStatus
            });
        }

        if (html) statsContainer.innerHTML = html;

    } catch (e) {
        console.warn("Error updating stats:", e);
    }
}

function renderStatGroup(title, data) {
    let rows = '';
    for (const [key, value] of Object.entries(data)) {
        let valClass = '';
        if (key === 'Packet Loss' && parseInt(value) > 0) valClass = 'warn';
        if (key === 'Est. Bandwidth') valClass = 'good';
        rows += `<div class="stat-row"><span class="stat-key">${key}:</span><span class="stat-val ${valClass}">${value}</span></div>`;
    }
    return `<div class="stat-group"><div class="stat-title">${title}</div>${rows}</div>`;
}

let isPaused = false;

function togglePauseStream() {
    if (!reader) {
        // startStream(); // 如果还没开始，就走完整流程
        return;
    }

    if (!isPaused) {
        // === 暂停 ===
        console.log("[Action] Pausing stream (Switching to Audio Only to save BW)...");
        layerSelect.disabled = true;
        
        // 1. 通过 WS 切到 Audio Only (假设 Audio ID 是 3)
        // 你需要从 abrEngine 获取 audioTrackId，这里假设我们存了
        if (abrEngine.audioTrackId !== null) {
            controlClient.selectLayer(abrEngine.audioTrackId, 'user_pause');
        }
        
        // 2. 停止 ABR 干扰
        abrEngine.setAutoMode(false);
        
        // 3. UI 变化
        video.style.opacity = '0.5'; // 变暗
        // statsContainer.innerHTML += '<div style="color:yellow">PAUSED (Audio Keep-alive)</div>';
        
        isPaused = true;
        document.getElementById('pauseBtn').innerText = "Resume";
    } else {
        // === 恢复 ===
        console.log("[Action] Resuming stream (Switching back to Auto)...");
        layerSelect.disabled = false;
        
        // 1. 恢复 ABR (它会自动切回最佳视频档)
        abrEngine.setAutoMode(true);
        
        // 2. 手动触发一次立即升级 (可选，加速恢复)
        // 比如直接切回 Video High (ID 0)
        controlClient.selectLayer(0, 'user_resume'); 
        
        // 3. UI 恢复
        video.style.opacity = '1';
        
        isPaused = false;
        document.getElementById('pauseBtn').innerText = "Pause";
    }
}

