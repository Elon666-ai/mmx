// mmxplayer.js

const ABR_CONFIG = {
    EMA_ALPHA: 0.15, 
    DOWNGRADE_LOSS: 0.10, 
    DOWNGRADE_RTT: 1500,
    DOWNGRADE_FPS: 10,
    DOWNGRADE_PERSISTENCE: 4,

    UPGRADE_LOSS: 0.05,   
    UPGRADE_RTT: 1000,     
    UPGRADE_FPS: 10,
    STABLE_DURATION: 5,  
    
    // --- 【修改】惩罚机制加强 ---
    BASE_PENALTY_TIME: 60000,  // 初次失败惩罚 60秒
    MAX_PENALTY_TIME: 300000,  // 最多惩罚 5分钟
    
    SWITCH_COOLDOWN: 8, 
    HONEYMOON_TIME: 20000 
};

const LEVELS = [
    { id: '1080p', label: '1080P (High)',   stream: 'd1080v', bitrate: 2000 },
    { id: '720p',  label: '720P (Std)',    stream: 'd720v',  bitrate: 1000 },
    { id: '540p',  label: '540P (Low)',    stream: 'd540v',  bitrate: 400 }, 
    { id: 'audio', label: 'Audio Only',    stream: 'audiov', bitrate: 64 } 
];

class MMXPlayer {
    constructor(videoElement, statsElement, baseUrl) {
        this.video = videoElement;
        this.statsEl = statsElement;
        this.baseUrl = baseUrl.endsWith('/') ? baseUrl : baseUrl + '/';
        
        this.currentReader = null;
        this.currentLevelIdx = 1; 
        this.isAuto = true;
        this.isSwitching = false;
        
        this.abrInterval = null;
        this.lastStats = null;
        
        this.stableCount = 0;
        this.badConditionCount = 0;
        this.switchCooldownTimer = 0;
        this.connectionStartTime = Date.now();
        
        this.penaltyBox = {};
        this.failureCounts = {}; // 记录每个档位的失败次数
        
        this.fpsHistory = []; 
        
        this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
        this.rawMetrics = { rtt: 0, packetLoss: 0, bitrate: 0, fps: 0, resolution: '' };
    }

    async start() {
        await this.loadLevel(this.currentLevelIdx);
        this.startStatsMonitor();
    }

    async loadLevel(index) {
        if (index < 0 || index >= LEVELS.length) return;
        
        // 允许错误重试通过，但阻断常规重复调用
        if (this.isSwitching && this.currentLevelIdx === index) return;

        const level = LEVELS[index];
        const isDowngrade = index > this.currentLevelIdx;
        const previousLevelIdx = this.currentLevelIdx;

        console.log(`[MMXPlayer] Attempting switch to ${level.label}...`);
        this.isSwitching = true;
        
        // --- 降级惩罚逻辑 ---
        if (isDowngrade) {
            // 1. 累加失败次数
            this.failureCounts[previousLevelIdx] = (this.failureCounts[previousLevelIdx] || 0) + 1;
            const count = this.failureCounts[previousLevelIdx];
            
            // 2. 指数计算: 60s, 120s, 240s...
            let penaltyTime = ABR_CONFIG.BASE_PENALTY_TIME * Math.pow(2, count - 1);
            if (penaltyTime > ABR_CONFIG.MAX_PENALTY_TIME) penaltyTime = ABR_CONFIG.MAX_PENALTY_TIME;

            // 3. 关进小黑屋
            const banUntil = Date.now() + penaltyTime;
            this.penaltyBox[previousLevelIdx] = banUntil;
            
            console.warn(`[ABR] 🚫 Banning ${LEVELS[previousLevelIdx].label} for ${penaltyTime/1000}s (Failures: ${count})`);
        }

        const whepUrl = new URL(`live/${level.stream}/whep`, this.baseUrl).toString();
        let trackHandled = false;

        const newReader = new MediaMTXWebRTCReader({
            url: whepUrl,
            onError: (err) => {
                console.error('[Reader Error]', err);
                this.isSwitching = false;

                if (this.isAuto && index < LEVELS.length - 1) {
                    console.warn(`[ABR] Connection failed, trying next level...`);
                    // 稍微延迟重试
                    setTimeout(() => this.loadLevel(index + 1), 200);
                }
            },
            onTrack: (evt) => {
                if (trackHandled) return;
                trackHandled = true;

                this.connectionStartTime = Date.now();

                const stream = evt.streams[0];
                console.log(`[MMXPlayer] ✅ Stream Ready: ${level.label}`);
                
                this.video.srcObject = stream;
                this.optimizeForLowLatency(stream);

                const playPromise = this.video.play();
                if (playPromise !== undefined) {
                    playPromise.catch(e => {
                        if (e.name !== 'AbortError') console.warn('Autoplay warning:', e);
                    });
                }

                if (this.currentReader && this.currentReader !== newReader) {
                    this.currentReader.close();
                }
                
                this.currentReader = newReader;
                this.currentLevelIdx = index;
                
                this.lastStats = null;
                this.fpsHistory = [];
                this.emaMetrics = { rtt: 0, packetLoss: 0, fps: 30 }; 
                this.stableCount = 0;
                this.badConditionCount = 0;
                
                this.isSwitching = false;
                this.switchCooldownTimer = ABR_CONFIG.SWITCH_COOLDOWN; 
            }
        });
    }

    optimizeForLowLatency(stream) {
        stream.getTracks().forEach(t => t.enabled = true);
        if (this.video.buffered.length > 0) {
            const end = this.video.buffered.end(this.video.buffered.length - 1);
            if (end - this.video.currentTime > 0.5) {
                this.video.currentTime = end - 0.1;
            }
        }
    }

    manualSwitch(levelId) {
        if (levelId === 'auto') {
            this.isAuto = true;
            console.log('[MMXPlayer] Mode: AUTO');
            return;
        }

        this.isAuto = false;
        const idx = LEVELS.findIndex(l => l.id === levelId);
        if (idx !== -1 && idx !== this.currentLevelIdx) {
            // 手动强制切换，清除该档位的失败记录
            this.failureCounts[idx] = 0;
            delete this.penaltyBox[idx];
            this.loadLevel(idx);
        }
    }

    startStatsMonitor() {
        if (this.abrInterval) clearInterval(this.abrInterval);

        this.abrInterval = setInterval(async () => {
            if (this.switchCooldownTimer > 0) this.switchCooldownTimer--;
            if (this.isSwitching || !this.currentReader || !this.currentReader.pc) return;

            try {
                const stats = await this.currentReader.pc.getStats();
                let activeCandidatePair = null;
                let videoRtp = null;
                let audioRtp = null;

                stats.forEach(report => {
                    if (report.type === 'candidate-pair' && report.state === 'succeeded') {
                        activeCandidatePair = report;
                    }
                    if (report.type === 'inbound-rtp') {
                        if (report.kind === 'video') videoRtp = report;
                        if (report.kind === 'audio') audioRtp = report;
                    }
                });

                if (activeCandidatePair) {
                    this.processStats(activeCandidatePair, videoRtp, audioRtp);
                    this.renderStats();
                    
                    if (this.isAuto && this.switchCooldownTimer === 0) {
                        this.decideQuality();
                    }
                }
            } catch (e) {
                console.warn("Stats error", e);
            }
        }, 1000);
    }

    processStats(pair, videoRtp, audioRtp) {
        const now = Date.now();
        const currentIdx = this.currentLevelIdx;
        const isAudioOnlyMode = currentIdx === LEVELS.length - 1;

        let primaryRtp = isAudioOnlyMode ? audioRtp : (videoRtp || audioRtp);
        if (!primaryRtp) {
            primaryRtp = { packetsLost: 0, packetsReceived: 0, bytesReceived: 0 };
        }

        const packetsLost = primaryRtp.packetsLost || 0;
        const packetsReceived = primaryRtp.packetsReceived || 0;
        const bytesReceived = primaryRtp.bytesReceived || 0;
        
        let instantLoss = 0;
        let instantBitrate = 0;
        let avgFPS = 30; 

        if (this.lastStats) {
            const deltaLost = packetsLost - this.lastStats.packetsLost;
            const deltaReceived = packetsReceived - this.lastStats.packetsReceived;
            const totalDelta = deltaLost + deltaReceived;
            
            if (totalDelta > 0) instantLoss = Math.max(0, deltaLost / totalDelta);

            const deltaBytes = bytesReceived - this.lastStats.bytesReceived;
            instantBitrate = (deltaBytes * 8) / 1000;

            if (isAudioOnlyMode) {
                this.fpsHistory = [];
                avgFPS = 30;
            } else if (videoRtp) {
                const framesDecoded = videoRtp.framesDecoded || 0;
                this.fpsHistory.push({ t: now, f: framesDecoded });
                
                const windowStart = now - 3000;
                this.fpsHistory = this.fpsHistory.filter(record => record.t >= windowStart);

                if (this.fpsHistory.length > 1) {
                    const first = this.fpsHistory[0];
                    const last = this.fpsHistory[this.fpsHistory.length - 1];
                    const timeDiff = (last.t - first.t) / 1000;
                    const frameDiff = last.f - first.f;
                    if (timeDiff > 0) avgFPS = frameDiff / timeDiff;
                }
            } else {
                this.fpsHistory = [];
                avgFPS = 0;
            }
        } else {
            avgFPS = isAudioOnlyMode ? 30 : 0;
        }

        const instantRtt = (pair.currentRoundTripTime || 0) * 1000;

        if (this.emaMetrics.rtt === 0) {
            this.emaMetrics.rtt = instantRtt;
            this.emaMetrics.packetLoss = instantLoss;
            this.emaMetrics.fps = avgFPS;
        } else {
            this.emaMetrics.rtt = (ABR_CONFIG.EMA_ALPHA * instantRtt) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.rtt);
            this.emaMetrics.packetLoss = (ABR_CONFIG.EMA_ALPHA * instantLoss) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.packetLoss);
            this.emaMetrics.fps = (ABR_CONFIG.EMA_ALPHA * avgFPS) + ((1 - ABR_CONFIG.EMA_ALPHA) * this.emaMetrics.fps);
        }

        this.rawMetrics = {
            rtt: instantRtt,
            packetLoss: instantLoss,
            bitrate: instantBitrate,
            fps: avgFPS,
            resolution: (videoRtp && videoRtp.frameWidth) ? `${videoRtp.frameWidth}x${videoRtp.frameHeight}` : 'Audio/None'
        };

        this.lastStats = { 
            packetsLost, packetsReceived, bytesReceived, 
            framesDecoded: (videoRtp ? (videoRtp.framesDecoded || 0) : 0),
            timestamp: now 
        };
    }

    decideQuality() {
        const { rtt, packetLoss, fps } = this.emaMetrics;
        const currentBitrate = this.rawMetrics.bitrate; 
        const currentIdx = this.currentLevelIdx;
        const levelConfig = LEVELS[currentIdx];
        const isAudioOnly = currentIdx === LEVELS.length - 1;

        const connectionDuration = Date.now() - this.connectionStartTime;
        const isHoneymoon = connectionDuration < ABR_CONFIG.HONEYMOON_TIME;

        let triggerReason = null;

        if (packetLoss > ABR_CONFIG.DOWNGRADE_LOSS) triggerReason = `Loss(${packetLoss.toFixed(2)})`;
        else if (rtt > ABR_CONFIG.DOWNGRADE_RTT) triggerReason = `RTT(${rtt.toFixed(0)})`;
        else if (!isAudioOnly && !isHoneymoon && fps < ABR_CONFIG.DOWNGRADE_FPS) triggerReason = `FPS(${fps.toFixed(1)})`;
        else if (!isAudioOnly && !isHoneymoon && currentBitrate > 0 && currentBitrate < levelConfig.bitrate * 0.7) triggerReason = `BW(${currentBitrate.toFixed(0)})`;

        const isVideoFrozen = !isAudioOnly && !isHoneymoon && (fps <= 1 || this.rawMetrics.fps <= 1);
        const isCritical = (this.rawMetrics.rtt > 3000) || isVideoFrozen;

        if (currentIdx < LEVELS.length - 1) {
            if (triggerReason || isCritical) {
                if (isCritical) {
                    let criticalReason = '';
                    if (this.rawMetrics.rtt > 3000) criticalReason = `CRITICAL_RTT(${this.rawMetrics.rtt})`;
                    else if (isVideoFrozen) criticalReason = `FROZEN(InstFPS:${this.rawMetrics.fps.toFixed(1)}, Bitrate:${currentBitrate.toFixed(0)})`;
                    
                    console.warn(`[ABR] 🚨 CRITICAL detected: ${criticalReason}. Emergency Downgrade!`);
                    this.loadLevel(currentIdx + 1);
                    this.badConditionCount = 0;
                    return;
                }

                this.badConditionCount++;
                console.debug(`[ABR] ⚠️ Bad condition: ${triggerReason}. Count: ${this.badConditionCount}/${ABR_CONFIG.DOWNGRADE_PERSISTENCE}`);
                
                if (this.badConditionCount >= ABR_CONFIG.DOWNGRADE_PERSISTENCE) {
                    console.warn(`[ABR] 📉 Network poor (${triggerReason}). Downgrading...`);
                    this.loadLevel(currentIdx + 1);
                    return;
                }
            } else {
                this.badConditionCount = 0;
            }
        }

        // --- 升级逻辑 ---
        if (currentIdx > 0) {
            const targetIdx = currentIdx - 1;
            // 检查惩罚箱
            if (this.isLevelBanned(targetIdx)) {
                this.stableCount = 0;
                return;
            }

            const isFpsHealthy = isAudioOnly || (fps >= ABR_CONFIG.UPGRADE_FPS);
            const isGoodNetwork = 
                (packetLoss <= ABR_CONFIG.UPGRADE_LOSS) && 
                (rtt < ABR_CONFIG.UPGRADE_RTT) &&
                isFpsHealthy;

            if (isGoodNetwork) {
                this.stableCount++;
                if (this.stableCount >= ABR_CONFIG.STABLE_DURATION) {
                    console.log(`[ABR] 📈 Network stable (RTT:${rtt.toFixed(0)}ms, Loss:${(packetLoss*100).toFixed(1)}%, FPS:${fps.toFixed(1)}). Upgrading to ${LEVELS[targetIdx].label}...`);
                    this.loadLevel(targetIdx);
                    this.stableCount = 0;
                }
            } else {
                this.stableCount = 0;
            }
        }
    }

    isLevelBanned(index) {
        const banUntil = this.penaltyBox[index];
        if (!banUntil) return false;
        
        if (Date.now() < banUntil) {
            return true;
        } else {
            // 时间到，解禁
            delete this.penaltyBox[index];
            console.log(`[ABR] ✅ Level ${LEVELS[index].label} penalty expired.`);
            return false;
        }
    }

    renderStats() {
        if (!this.statsEl) return;
        const level = LEVELS[this.currentLevelIdx];
        const mode = this.isAuto ? `<span style="color:#0f0">AUTO</span>` : `<span style="color:#fa0">MANUAL</span>`;
        
        let bannedInfo = '';
        Object.keys(this.penaltyBox).forEach(idx => {
            const left = Math.ceil((this.penaltyBox[idx] - Date.now()) / 1000);
            if (left > 0) {
                const fails = this.failureCounts[idx] || 0;
                bannedInfo += `<div style="color:red;font-size:12px">🚫 ${LEVELS[idx].id} banned: ${left}s (Fail x${fails})</div>`;
            }
        });

        const downgradeProgress = this.isAuto && this.currentLevelIdx < LEVELS.length-1 
            ? `(Bad: ${this.badConditionCount}/${ABR_CONFIG.DOWNGRADE_PERSISTENCE})` : '';
        const upgradeProgress = this.isAuto && this.currentLevelIdx > 0 
            ? `(Stable: ${this.stableCount}/${ABR_CONFIG.STABLE_DURATION})` : '';

        const fpsColor = (!level.id.includes('audio') && this.rawMetrics.fps < 10) ? 'red' : 'white';
        const honeymoon = (Date.now() - this.connectionStartTime < ABR_CONFIG.HONEYMOON_TIME) ? '🛡️' : '';

        this.statsEl.innerHTML = `
            <div><strong>Mode:</strong> ${mode} ${honeymoon}</div>
            <div><strong>Stream:</strong> ${level.label}</div>
            <div><strong>Bitrate:</strong> ${this.rawMetrics.bitrate.toFixed(0)} kbps</div>
            <hr style="border-color:#555">
            <div><strong>FPS (3s Avg):</strong> <span style="color:${fpsColor}">${this.rawMetrics.fps.toFixed(1)}</span></div>
            <div><strong>RTT (EMA):</strong> ${this.emaMetrics.rtt.toFixed(0)} ms</div>
            <div><strong>Loss (EMA):</strong> ${(this.emaMetrics.packetLoss * 100).toFixed(2)}%</div>
            <div><strong>Trend:</strong> <span style="font-size:12px">${downgradeProgress} ${upgradeProgress}</span></div>
            ${bannedInfo}
        `;
    }

    destroy() {
        if (this.abrInterval) clearInterval(this.abrInterval);
        if (this.currentReader) this.currentReader.close();
    }
}