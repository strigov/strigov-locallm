// poller.js — опрос /api/admin/state (§10.3): раз в interval мс, только при видимой вкладке, без наложения запросов.
// Первый ответ 401 останавливает опрос навсегда (F3-D1): новый опрос начинается только после нового входа.

export class Poller {
  // fetchState — промис состояния; onState(st); onError(err) — прочие ошибки; onAuthLost(err) — первый 401;
  // env — {setTimer(fn, ms), clearTimer(id), visible()}.
  constructor({ fetchState, onState, onError, onAuthLost, interval = 1000, env }) {
    this.fetchState = fetchState;
    this.onState = onState;
    this.onError = onError || (() => {});
    this.onAuthLost = onAuthLost || (() => {});
    this.interval = interval;
    this.env = env;
    this.timer = null;
    this.busy = false;
    this.running = false;
    this.dead = false;
  }

  start() {
    if (this.running || this.dead) return;
    this.running = true;
    this.tick();
  }

  stop() {
    this.running = false;
    if (this.timer !== null) {
      this.env.clearTimer(this.timer);
      this.timer = null;
    }
  }

  // visibilityChanged — вкладка снова видна: опрос сразу, если запрос не идёт и таймер не ждёт.
  visibilityChanged() {
    if (this.running && this.env.visible() && !this.busy && this.timer === null) this.tick();
  }

  schedule() {
    if (!this.running || this.timer !== null) return;
    this.timer = this.env.setTimer(() => {
      this.timer = null;
      this.tick();
    }, this.interval);
  }

  async tick() {
    if (!this.running || this.busy || !this.env.visible()) return;
    this.busy = true;
    try {
      const st = await this.fetchState();
      if (this.running) this.onState(st);
    } catch (err) {
      if (err && err.status === 401) {
        this.dead = true;
        this.stop();
        this.onAuthLost(err);
        return;
      }
      if (this.running) this.onError(err);
    } finally {
      this.busy = false;
    }
    this.schedule();
  }
}
