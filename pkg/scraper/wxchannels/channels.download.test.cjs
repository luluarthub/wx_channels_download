// Run: node --test pkg/scraper/wxchannels/channels.download.test.cjs
// Loads the production scripts; only DOM, menu widgets and network boundaries are mocked.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const test = require("node:test");

class Element {
  constructor(className = "", text = "", attrs = {}) {
    this.nodeType = 1;
    this.tagName = "DIV";
    this.className = className;
    this.text = text;
    this.attrs = attrs;
    this.children = [];
    this.style = {};
    this.listeners = {};
    this.rect = { top: 0, left: 0, right: 800, bottom: 600 };
  }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  get childNodes() {
    return [...(this.text ? [{ nodeType: 3, textContent: this.text }] : []), ...this.children];
  }
  getAttribute(key) { return this.attrs[key] || null; }
  appendChild(child) { this.children.push(child); child.parentElement = this; return child; }
  matches(selector) {
    return selector === "*" || selector.split(",").some((s) =>
      s.trim().startsWith(".") && this.className.split(" ").includes(s.trim().slice(1)));
  }
  closest(selector) {
    return this.matches(selector) ? this : this.parentElement ? this.parentElement.closest(selector) : null;
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [
      ...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector),
    ]);
  }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  getBoundingClientRect() { return this.rect; }
}

function feed(id, title = `Video ${id}`, specs = ["xWT111", "xWT113"]) {
  return {
    id, objectNonceId: `nonce-${id}`,
    contact: { username: "author", nickname: "Author", headUrl: "avatar" },
    objectDesc: {
      description: title, mediaType: 4,
      media: [{ url: `https://cdn.example/${id}?encfilekey=f`,
        urlToken: "&token=t&basedata=a%2Fb%3D&sign=a+b%2F&svrbypass=c%26d&svrnonce=123",
        decodeKey: "", coverUrl: `https://cdn.example/${id}.jpg`,
        spec: specs.map((fileFormat) => ({ fileFormat })) }],
    },
  };
}

function harness() {
  const body = new Element();
  const events = new Map();
  const requests = [], errors = [], fetched = [], saved = [];
  const metrics = { hidden: 0, played: 0 };
  const log = () => {};
  const chain = new Proxy({}, { get: () => () => chain });
  log.Info = () => chain;
  const on = (event, callback) => {
    if (!events.has(event)) events.set(event, []);
    events.get(event).push(callback);
  };
  const WXU = {
    log, on, onAPILoaded() {}, onUtilsLoaded() {}, onInit() {},
    onDOMContentLoaded(fn) { fn(); }, observe_node() {},
    emit() {}, toast() {}, error(error) { errors.push(error); },
    config: { downloadInFrontend: false, defaultHighest: false },
    Events: { BeforeDownloadMedia: "before", MediaDownloaded: "media", MP3Downloaded: "mp3" },
    downloader: {
      browse() {}, show() {},
      async create(feeds, options) { requests.push({ feeds, options }); return [null, {}]; },
    },
    remove_zero: String,
    loading() { return { hide() { metrics.hidden++; } }; },
    async fetch(url) { fetched.push(url); return [null, { async blob() { return new Blob(["cover"]); } }]; },
    async download_with_progress() { return new Blob(["video"]); },
    async media_to_mp3() { return [null, new Blob(["mp3"])]; },
    save(blob, name) { saved.push({ blob, name }); },
    bytes_to_size: String,
  };
  for (const name of ["FetchFeedProfile", "PCFlowLoaded", "RecommendFeedsLoaded", "UserFeedsLoaded",
    "GotoNextFeed", "GotoPrevFeed", "HomeFeedChanged"]) {
    WXU[`on${name}`] = (fn) => on(name, fn);
  }
  class Menu {
    constructor(options) { Object.assign(this, options); }
    hide() {} setReference() {} handleEnterTrigger() {} handleLeaveTrigger() {}
    setItems(items) { this.items = items; }
  }
  const context = vm.createContext({
    WXU, WXE: { Events: { Feed: "feed" } },
    document: { body, createElement() { return new Element(); },
      querySelectorAll(selector) { return body.querySelectorAll(selector); } },
    window: { innerWidth: 800, innerHeight: 600, __d_config: { version: "test" }, atob, btoa },
    location: { href: "https://channels.weixin.qq.com/web/pages/feed" },
    Timeless: { vm: { MenuCore: Menu, MenuItemCore: Menu, DropdownMenuCore: Menu },
      DOM: { render() {} }, weui: { DropdownMenu() {} } },
    URL, Blob, Uint8Array, setTimeout() { return 1; }, clearTimeout() {}, console: { log() {} },
  });
  function load(name) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, "inject", name), "utf8"), context, { filename: name });
  }
  load("channels.utils.js");
  WXU.play_cur_video = () => metrics.played++;
  function card(item, withID = true) {
    const slide = body.appendChild(new Element("slides-item", "", withID ? { "data-id": item.id } : {}));
    slide.appendChild(new Element("description", item.objectDesc.description));
    const trigger = slide.appendChild(new Element("download-icon"));
    return { slide, trigger };
  }
  function menu(trigger) {
    const dropdown = WXU.attach_download_dropdown_menu(trigger);
    trigger.listeners.mouseenter();
    return dropdown;
  }
  function emit(event, data) { for (const callback of events.get(event) || []) callback(data); }
  return { WXU, body, context, requests, errors, fetched, saved, metrics, card, menu, emit, load };
}

test("scrolling selects the clicked card instead of a stale global feed", async () => {
  const h = harness(), old = feed("1"), current = feed("11");
  h.WXU.set_feed(old);
  h.WXU.cache_feeds([current]);
  const { trigger } = h.card(current);
  await h.WXU.downloadBtnHandler({ currentTarget: trigger });
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].feeds[0].id, "11");
  assert.equal(h.requests[0].options.spec, "xWT111");
});

test("unknown ID, ambiguous title, title prefix and missing card never use the old global feed", async () => {
  for (const kind of ["unknown", "duplicate", "prefix", "missing"]) {
    const h = harness(), old = feed("1", "This title is shared");
    h.WXU.set_feed(old);
    let trigger;
    if (kind === "missing") trigger = new Element("download-icon");
    else {
      const item = feed("11", kind === "prefix" ? "This title is shared but different" : old.objectDesc.description);
      ({ trigger } = h.card(item, kind === "unknown"));
      if (kind === "duplicate") h.WXU.cache_feeds(item);
    }
    await h.WXU.downloadBtnHandler({ currentTarget: trigger });
    assert.equal(h.requests.length, 0, kind);
    assert.equal(h.errors.length, 1, kind);
  }
});

test("complete unique titles and nonce attributes resolve a card", () => {
  const h = harness(), item = feed("42", "完整的视频标题");
  h.WXU.cache_feeds(item);
  const { slide, trigger } = h.card(item, false);
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), item);
  slide.attrs["data-nonce-id"] = item.objectNonceId;
  slide.children[0].text = "truncated...";
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), item);
});

// Captured public templates: FinderHome FeedDesc renders UserText fragments in
// CollapsedText .ctn, followed by a hidden .more-btn containing "收起". Its
// .compute-node contains only the first line and is not the video's identity.
function collapsedDescription(slide, fragments, firstLine) {
  slide.children = [];
  slide.appendChild(new Element("author", "Visible author"));
  const description = slide.appendChild(new Element("collapsed-text content select-none"));
  const content = description.appendChild(new Element("ctn ctn--col"));
  for (const fragment of fragments) content.appendChild(fragment);
  content.appendChild(new Element("click-box more-btn float-right", "收起"));
  description.appendChild(new Element("click-box more-btn", "展开"));
  description.appendChild(new Element("compute-node", firstLine));
  const trigger = slide.appendChild(new Element("download-icon", "下载"));
  return { content, trigger };
}

test("upstream collapsed rich text resolves the complete multiline description without UI labels", async () => {
  const h = harness();
  const current = feed("current", "第一行完整的问题？\n**第一段正文**\n第二段 #话题\n");
  h.WXU.set_feed(feed("old", "Other video"));
  h.WXU.cache_feeds(current);
  const { slide } = h.card(current, false);
  const { trigger } = collapsedDescription(slide, [
    new Element("", "第一行完整的问题？\n"),
    new Element("text--hl", "**第一段正文**\n"),
    new Element("", "第二段 "), new Element("text--lk", "#话题\n"),
  ], "第一行完整的问题？");
  await h.WXU.downloadBtnHandler({ currentTarget: trigger });
  assert.equal(h.errors.length, 0);
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].feeds[0].id, current.id);
});

test("generic clickable descriptions remain eligible while title prefixes and duplicate titles stay unsafe", () => {
  const h = harness(), current = feed("current", "Complete unique description");
  h.WXU.cache_feeds(current);
  const { slide, trigger } = h.card(current, false);
  slide.children[0].className = "click-box description";
  slide.appendChild(new Element("author", "Visible author"));
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), current);
  slide.children[0].text = "Complete unique";
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), null);
  slide.children[0].text = current.objectDesc.description;
  h.WXU.cache_feeds(feed("other", current.objectDesc.description));
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), null);
});

test("a hidden first-line measurement never selects an old feed sharing the prefix", () => {
  const h = harness(), old = feed("old", "Shared first line"), current = feed("current", "Shared first line\nNew complete content");
  h.WXU.set_feed(old); h.WXU.cache_feeds(current);
  const { slide } = h.card(current, false);
  const { content, trigger } = collapsedDescription(slide, [
    new Element("", "Shared first line"), new Element("text--hl", "\nNew complete content"),
  ], "Shared first line");
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), current);
  content.children = [new Element("", "Shared first line...")];
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), null);
});

test("collapsed descriptions preserve emoji alt text and reject conflicting explicit video IDs", () => {
  const h = harness(), current = feed("current", "Text[微笑]End");
  h.WXU.cache_feeds(current);
  const { slide } = h.card(current, false);
  const emoji = new Element("emoji", "", { alt: "[微笑]" });
  emoji.tagName = "IMG";
  const { trigger } = collapsedDescription(slide, [new Element("", "Text"), emoji, new Element("", "End")], "Text");
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), current);
  slide.attrs["data-object-id"] = "not-loaded";
  assert.equal(h.WXU.resolve_feed_from_trigger(trigger), null);
});

test("all menu downloads re-resolve the reused card at click time", async () => {
  for (const action of ["下载为MP3", "下载封面", "原始视频", "xWT111"]) {
    const h = harness(), old = feed("1"), current = feed("2");
    h.WXU.set_feed(old);
    h.WXU.cache_feeds(current);
    const { slide, trigger } = h.card(old);
    const menu = h.menu(trigger);
    const items = menu.items.concat(menu.items.find((item) => item.label === "更多下载").menu.items);
    slide.attrs["data-id"] = current.id;
    slide.children[0].text = current.objectDesc.description;
    items.find((item) => item.label === action).onClick();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(h.requests.length, 1, action);
    assert.equal(h.requests[0].feeds[0].id, current.id, action);
    if (action === "下载为MP3") assert.equal(h.requests[0].options.suffix, ".mp3");
    if (action === "下载封面") assert.equal(h.requests[0].options.suffix, ".jpg");
    if (action === "原始视频") assert.equal(h.requests[0].options.spec, "original");
  }
});

test("stale quality menu cannot submit a rendition missing from the new video", async () => {
  const h = harness(), old = feed("1"), current = feed("2", "Other video", ["xNEW"]);
  h.WXU.set_feed(old); h.WXU.cache_feeds(current);
  const { slide, trigger } = h.card(old);
  const items = h.menu(trigger).items.find((item) => item.label === "更多下载").menu.items;
  slide.attrs["data-id"] = current.id;
  items.find((item) => item.label === "xWT111").onClick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.requests.length, 0);
  assert.equal(h.errors.length, 1);
});

test("floating button uses visible card and legacy button uses address identity", async () => {
  const h = harness(), old = feed("1"), current = feed("2");
  h.WXU.set_feed(old); h.WXU.cache_feeds(current);
  h.card(old).slide.rect = { top: -600, bottom: 0, left: 0, right: 800 };
  h.card(current);
  const trigger = h.body.appendChild(new Element("wx-sider-tools-btn"));
  await h.WXU.downloadBtnHandler({ currentTarget: trigger });
  assert.equal(h.requests[0].feeds[0].id, "2");
  h.body.children = [trigger];
  h.context.location.href += "?objectId=2";
  await h.WXU.downloadBtnHandler({ currentTarget: trigger });
  assert.equal(h.requests[1].feeds[0].id, "2");
  h.context.location.href = "https://channels.weixin.qq.com/web/pages/feed?oid=" + h.WXU.Base64.encodeUint64ToBase64("2");
  await h.WXU.downloadBtnHandler({ currentTarget: trigger });
  assert.equal(h.requests[2].feeds[0].id, "2");
});

test("cache preserves different videos with the same title and keeps its bounded recent window", () => {
  const h = harness();
  const first = feed("first", "Same title"), second = feed("second", "Same title");
  h.WXU.cache_feeds([first, second]);
  assert.equal(h.context.__wx_channels_store__.feeds.length, 2);
  for (let i = 0; i < 205; i++) h.WXU.cache_feeds(feed(String(i)));
  assert.equal(h.context.__wx_channels_store__.feeds.length, 200);
  assert.equal(h.context.__wx_channels_store__.feeds[0].id, "5");
  h.WXU.cache_feeds(feed("204", "Refreshed"));
  assert.equal(h.context.__wx_channels_store__.feeds.length, 200);
  assert.equal(h.context.__wx_channels_store__.feeds.at(-1).objectDesc.description, "Refreshed");
});

test("home and detail scripts cache preload, recommendations and later details without replacing current video", () => {
  for (const script of ["channels.home.js", "channels.feed.js"]) {
    const h = harness(); h.load(script);
    h.emit("channels:PreloadFeeds", []);
    h.emit("PCFlowLoaded", []);
    const first = feed("first"), detail = feed("detail"), recommendation = feed("recommendation"), user = feed("user");
    h.emit("FetchFeedProfile", first);
    h.emit("FetchFeedProfile", detail);
    h.emit("RecommendFeedsLoaded", [recommendation]);
    h.emit("UserFeedsLoaded", [user]);
    h.emit("channels:PreloadFeeds", [feed("preloaded")]);
    assert.equal(h.context.__wx_channels_store__.feed.id, first.id);
    for (const item of [detail, recommendation, user]) {
      assert.equal(h.WXU.resolve_feed_from_trigger(h.card(item).trigger), item);
    }
    h.emit("HomeFeedChanged", detail);
    assert.equal(h.context.__wx_channels_store__.feed.id, detail.id);
  }
});

test("original URLs keep signed bytes; explicit quality replaces only the rendition parameter", () => {
  const h = harness();
  const signed = "https://cdn.example/video?encfilekey=f&token=a%2Fb%3D&basedata=1&sign=a+b%2F&svrbypass=c%26d&svrnonce=2";
  for (const spec of [undefined, "", "original"]) assert.equal(h.WXU.build_download_url(signed, spec), signed);
  assert.equal(h.WXU.build_download_url(signed, "xWT111"), signed + "&X-snsvideoflag=xWT111");
  assert.equal(h.WXU.build_download_url(signed + "&X-snsvideoflag=old#fragment", "xWT113"), signed + "&X-snsvideoflag=xWT113#fragment");
  assert.equal(h.WXU.build_download_url("https://cdn.example/video", "xWT111"), "https://cdn.example/video?X-snsvideoflag=xWT111");
  assert.equal(h.WXU.build_download_url("zip://weixin.qq.com?files=[]", "xWT111"), "zip://weixin.qq.com?files=[]");
});

test("frontend original menu fetches intact signed URL and leaves cached raw feed unchanged", async () => {
  const h = harness(), item = feed("1");
  h.WXU.config.downloadInFrontend = true;
  h.WXU.set_feed(item);
  const before = JSON.stringify(item);
  const { trigger } = h.card(item);
  const items = h.menu(trigger).items.find((entry) => entry.label === "更多下载").menu.items;
  items.find((entry) => entry.label === "原始视频").onClick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(h.fetched[0], item.objectDesc.media[0].url + item.objectDesc.media[0].urlToken);
  assert.equal(h.saved.length, 1);
  assert.equal(h.metrics.hidden, 1);
  assert.equal(JSON.stringify(item), before);
});

test("frontend network failure closes loading state and resumes playback", async () => {
  const h = harness(), item = feed("1");
  h.WXU.config.downloadInFrontend = true;
  h.WXU.config.downloadPauseWhenDownload = true;
  h.WXU.fetch = async () => [new Error("network failure"), null];
  h.WXU.set_feed(item);
  await h.WXU.downloadBtnHandler({ currentTarget: h.card(item).trigger });
  assert.equal(h.metrics.hidden, 1);
  assert.equal(h.metrics.played, 1);
  assert.equal(h.errors[0].msg, "network failure");
});

test("frontend MP3 and cover fetch the clicked feed and do not mutate the cached object", async () => {
  for (const action of ["下载为MP3", "下载封面"]) {
    const h = harness(), old = feed("1"), item = feed("2");
    h.WXU.config.downloadInFrontend = true;
    h.WXU.set_feed(old); h.WXU.cache_feeds(item);
    const before = JSON.stringify(item);
    const { trigger } = h.card(item);
    h.menu(trigger).items.find((entry) => entry.label === action).onClick();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(h.fetched[0], action === "下载封面"
      ? item.objectDesc.media[0].coverUrl
      : item.objectDesc.media[0].url + item.objectDesc.media[0].urlToken);
    assert.equal(h.saved.length, 1, action);
    assert.equal(JSON.stringify(item), before, action);
  }
});

test("picture and live formatting remain available", () => {
  const h = harness(), picture = feed("picture");
  picture.objectDesc.mediaType = 2;
  const profile = h.WXU.format_feed(picture);
  assert.equal(profile.type, "picture");
  assert.ok(h.WXU.build_picture_zip_url(profile).startsWith("zip://"));
  const live = h.WXU.format_feed({ liveInfo: { streamUrl: "https://live.example/stream" }, contact: picture.contact });
  assert.equal(live.type, "live");
  assert.equal(live.url, "https://live.example/stream");
});
