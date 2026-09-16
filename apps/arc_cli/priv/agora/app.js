(() => {
  "use strict";

  const $ = (selector) => document.querySelector(selector);
  const elements = {
    app: $("#app"),
    unauthorized: $("#unauthorized"),
    connection: $("#connection"),
    boardDescription: $("#board-description"),
    citizenName: $("#citizen-name"),
    citizenKey: $("#citizen-key"),
    boardName: $("#board-name"),
    boardKey: $("#board-key"),
    composerSection: $("#composer-section"),
    composerTitle: $("#composer-title"),
    replyContext: $("#reply-context"),
    form: $("#composer-form"),
    body: $("#post-body"),
    byteCount: $("#byte-count"),
    postButton: $("#post-button"),
    composerError: $("#composer-error"),
    feedEyebrow: $("#feed-eyebrow"),
    feedTitle: $("#feed-title"),
    feedStatus: $("#feed-status"),
    posts: $("#posts"),
    more: $("#more-button"),
    refresh: $("#refresh-button"),
    back: $("#back-button"),
    template: $("#post-template"),
    lockedTitle: $("#unauthorized h2"),
    lockedText: $("#unauthorized p:not(.eyebrow)"),
    lockedCommand: $("#unauthorized code")
  };

  const state = {
    session: null,
    view: { kind: "feed", id: null, next: null },
    posts: [],
    parent: null,
    loading: false,
    submitting: false,
    requestId: null,
    requestBody: null,
    requestParent: null
  };

  const encoder = new TextEncoder();

  function byteLength(text) {
    return encoder.encode(text).length;
  }

  function shortKey(key) {
    if (typeof key !== "string" || key.length < 16) return "";
    return `${key.slice(0, 8)}…${key.slice(-8)}`;
  }

  function authorName(post) {
    return `Citizen ${shortKey(post.author)}`;
  }

  function formatTime(value) {
    const date = new Date(Number(value) * 1000);
    if (Number.isNaN(date.getTime())) return null;
    return new Intl.DateTimeFormat(undefined, {
      dateStyle: "medium",
      timeStyle: "short"
    }).format(date);
  }

  function setStatus(message, isError = false) {
    elements.feedStatus.textContent = message || "";
    elements.feedStatus.classList.toggle("error-state", isError);
  }

  function setComposerError(message) {
    elements.composerError.textContent = message || "";
    elements.composerError.hidden = !message;
  }

  function updateByteCount() {
    const used = byteLength(elements.body.value);
    const max = state.session?.limits?.body_bytes || 4096;
    elements.byteCount.textContent = `${used} / ${max} bytes`;
    elements.byteCount.classList.toggle("error-state", used > max);
    elements.postButton.disabled = state.submitting || state.loading || used === 0 || used > max;
    if (state.requestBody !== null && elements.body.value !== state.requestBody) {
      state.requestId = null;
      state.requestBody = null;
      state.requestParent = null;
    }
  }

  function syncControls() {
    const blocked = state.loading || state.submitting;
    elements.body.disabled = state.submitting;
    elements.refresh.disabled = blocked;
    elements.more.disabled = blocked;
    elements.back.disabled = blocked;
    for (const button of elements.posts.querySelectorAll("button")) button.disabled = blocked;
    updateByteCount();
  }

  function canNavigate() {
    if (state.loading || state.submitting) return false;
    if (state.requestId) {
      setComposerError("Retry or edit your draft before changing conversations.");
      return false;
    }
    return true;
  }

  async function request(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.body) headers.set("Content-Type", "application/json");
    headers.set("Accept", "application/json");
    const response = await fetch(path, {
      ...options,
      headers,
      credentials: "same-origin"
    });
    let payload = null;
    try { payload = await response.json(); } catch (_) { /* handled below */ }
    if (!response.ok) {
      const error = new Error(payload?.error || `Request failed (${response.status})`);
      error.status = response.status;
      throw error;
    }
    return payload;
  }

  function clearPosts() {
    elements.posts.replaceChildren();
  }

  function empty(message) {
    const paragraph = document.createElement("p");
    paragraph.className = "empty";
    paragraph.textContent = message;
    elements.posts.replaceChildren(paragraph);
  }

  function postElement(post, { parent = false } = {}) {
    const fragment = elements.template.content.cloneNode(true);
    const article = fragment.querySelector("article");
    const author = fragment.querySelector(".post__author");
    const key = fragment.querySelector(".post__key");
    const time = fragment.querySelector(".post__time");
    const body = fragment.querySelector(".post__body");
    const reply = fragment.querySelector(".post__reply");
    const thread = fragment.querySelector(".post__thread");

    article.dataset.postId = post.id;
    author.textContent = authorName(post);
    key.textContent = shortKey(post.author);
    key.title = post.author || "";
    const signedTime = formatTime(post.created_at);
    if (signedTime) {
      time.dateTime = new Date(Number(post.created_at) * 1000).toISOString();
      time.textContent = `Signed ${signedTime}`;
    } else {
      time.textContent = "Signed time unavailable";
    }
    body.textContent = post.body || "";
    reply.addEventListener("click", () => chooseReply(post));
    thread.addEventListener("click", () => openThread(post.id));
    if (parent) thread.hidden = true;
    return fragment;
  }

  function renderPosts() {
    clearPosts();
    if (state.view.kind === "thread" && state.parent) {
      const label = document.createElement("p");
      label.className = "eyebrow";
      label.textContent = "Original post";
      elements.posts.append(label, postElement(state.parent, { parent: true }));
      const repliesLabel = document.createElement("p");
      repliesLabel.className = "eyebrow replies-label";
      repliesLabel.textContent = "Direct replies";
      elements.posts.append(repliesLabel);
    }
    if (state.posts.length === 0) {
      const message = state.view.kind === "thread"
        ? "No direct replies yet. Add the first reply above."
        : "No public posts yet. Be the first to make a record.";
      const paragraph = document.createElement("p");
      paragraph.className = "empty";
      paragraph.textContent = message;
      elements.posts.append(paragraph);
    } else {
      for (const post of state.posts) elements.posts.append(postElement(post));
    }
    elements.more.hidden = state.view.next === null || state.view.next === undefined;
    syncControls();
  }

  function renderView() {
    const isThread = state.view.kind === "thread";
    elements.feedEyebrow.textContent = isThread
      ? "Conversation · oldest accepted replies first"
      : "Board record · oldest accepted posts first";
    elements.feedTitle.textContent = isThread ? "Thread" : "Public posts";
    elements.back.hidden = !isThread;
    elements.composerTitle.textContent = isThread ? "Reply to this thread" : "Speak to the agora";
    elements.postButton.textContent = isThread ? "Publish reply" : "Publish post";
    elements.replyContext.hidden = !isThread;
    elements.replyContext.textContent = isThread
      ? "Your reply will be public and attached directly to this post."
      : "";
    renderPosts();
  }

  function chooseReply(post) {
    if (!canNavigate()) return;
    state.view = { kind: "thread", id: post.id, next: null };
    state.parent = post;
    state.posts = [];
    state.requestId = null;
    state.requestBody = null;
    state.requestParent = null;
    renderView();
    elements.composerSection.scrollIntoView({ block: "start", behavior: "auto" });
    elements.body.focus();
    load({ reset: true });
  }

  async function openThread(id) {
    if (!id || !canNavigate()) return;
    state.view = { kind: "thread", id, next: null };
    state.parent = null;
    state.posts = [];
    state.requestId = null;
    state.requestBody = null;
    state.requestParent = null;
    renderView();
    await load({ reset: true });
  }

  function showFeed() {
    if (!canNavigate()) return;
    state.view = { kind: "feed", id: null, next: null };
    state.parent = null;
    state.posts = [];
    state.requestId = null;
    state.requestBody = null;
    state.requestParent = null;
    renderView();
    load({ reset: true });
  }

  function uniquePosts(posts) {
    const seen = new Set();
    return posts.filter((post) => {
      if (!post?.id || seen.has(post.id)) return false;
      seen.add(post.id);
      return true;
    });
  }

  async function load({ reset = false, more = false, whileSubmitting = false } = {}) {
    if (!state.session || state.loading || (state.submitting && !whileSubmitting)) return;
    state.loading = true;
    elements.posts.setAttribute("aria-busy", "true");
    syncControls();
    if (reset) setStatus("Reading the board…");
    if (more) setStatus("Loading more posts…");
    try {
      const after = more ? state.view.next : null;
      const params = new URLSearchParams({ limit: "20" });
      if (after !== null && after !== undefined) params.set("after", String(after));
      const endpoint = state.view.kind === "thread"
        ? `/api/thread?id=${encodeURIComponent(state.view.id)}&${params}`
        : `/api/feed?${params}`;
      const data = await request(endpoint);
      if (state.view.kind === "thread") state.parent = data.post;
      state.posts = uniquePosts(more ? state.posts.concat(data.posts || []) : (data.posts || []));
      state.view.next = data.next ?? null;
      setStatus("");
      renderView();
      return true;
    } catch (error) {
      setStatus(error.message || "Could not read this board.", true);
      if (reset) empty("The board could not be read. Try refreshing.");
      return false;
    } finally {
      state.loading = false;
      elements.posts.setAttribute("aria-busy", "false");
      syncControls();
    }
  }

  async function submit(event) {
    event.preventDefault();
    if (state.submitting || state.loading || !state.session) return;
    const body = elements.body.value;
    const limit = state.session.limits?.body_bytes || 4096;
    if (body.trim().length === 0) return setComposerError("Write something before publishing.");
    if (byteLength(body) > limit) return setComposerError(`Posts may contain at most ${limit} bytes.`);

    const parent = state.view.kind === "thread" ? state.view.id : null;
    if (state.requestBody !== body || state.requestParent !== parent || !state.requestId) {
      state.requestId = crypto.randomUUID();
      state.requestBody = body;
      state.requestParent = parent;
    }
    state.submitting = true;
    setComposerError("");
    updateByteCount();
    syncControls();
    elements.postButton.textContent = "Publishing…";
    try {
      await request("/api/posts", {
        method: "POST",
        body: JSON.stringify({ body, parent, request_id: state.requestId })
      });
      elements.body.value = "";
      state.requestId = null;
      state.requestBody = null;
      state.requestParent = null;
      updateByteCount();
      const refreshed = await load({ reset: true, whileSubmitting: true });
      if (refreshed) {
        setStatus(state.view.next === null
          ? "Published to this board."
          : "Published to this board. Load more to reach the newest posts.");
      } else {
        setStatus("Published, but the board could not be refreshed. Try Refresh.", true);
      }
    } catch (error) {
      setComposerError(error.message || "Could not publish. Your draft is still here; try again.");
    } finally {
      state.submitting = false;
      elements.postButton.textContent = state.view.kind === "thread" ? "Publish reply" : "Publish post";
      updateByteCount();
      syncControls();
    }
  }

  async function establishSession() {
    const fragment = new URLSearchParams(window.location.hash.slice(1));
    const token = fragment.get("token");
    if (token) {
      history.replaceState(null, "", `${window.location.pathname}${window.location.search}`);
      await request("/api/session", { method: "POST", body: JSON.stringify({ token }) });
    }
    return request("/api/session");
  }

  function renderSession(session) {
    state.session = session;
    const citizen = session.citizen || {};
    const board = session.board || {};
    const connection = session.connection || {};
    elements.citizenName.textContent = citizen.name || "ARC citizen";
    elements.citizenKey.textContent = shortKey(citizen.public_key);
    elements.citizenKey.title = citizen.public_key || "";
    elements.boardName.textContent = board.name || "Agora board";
    elements.boardKey.textContent = shortKey(board.public_key);
    elements.boardKey.title = board.public_key || "";
    elements.boardDescription.textContent = `A public record on ${board.name || "this board"}.`;
    elements.connection.hidden = false;
    elements.connection.dataset.mode = connection.mode || "local";
    elements.connection.textContent = connection.label || (connection.mode === "relay" ? "Connected through ARC relay" : "Connected locally");
    elements.app.hidden = false;
    updateByteCount();
  }

  async function start() {
    elements.body.addEventListener("input", updateByteCount);
    elements.form.addEventListener("submit", submit);
    elements.refresh.addEventListener("click", () => load({ reset: true }));
    elements.more.addEventListener("click", () => load({ more: true }));
    elements.back.addEventListener("click", showFeed);
    try {
      renderSession(await establishSession());
      await load({ reset: true });
    } catch (error) {
      if (error.status === 401 || error.status === 403) {
        elements.boardDescription.textContent = "This local view opens through your ARC command.";
        elements.unauthorized.hidden = false;
      } else {
        elements.boardDescription.textContent = "Agora could not start its local session.";
        elements.lockedTitle.textContent = "Agora could not start";
        elements.lockedText.textContent = error.message || "Could not connect to the local ARC service.";
        elements.lockedCommand.hidden = true;
        elements.unauthorized.hidden = false;
      }
    }
  }

  start();
})();
