import { ReactNode, useState } from "react";
import { Logo } from "./ui";

/*
 * Mock renderings of a webhook payload as Discord and Slack would show it.
 * They render the exact JSON the server would send (from
 * POST /api/webhooks/preview), so what you see is what gets delivered,
 * give or take each client's own fonts and markdown quirks.
 */

// ---- tiny inline markdown engine ----

interface Rule {
  re: RegExp; // must be sticky-free, global-free; matched with .exec on the remaining text
  render: (m: RegExpExecArray, key: number) => ReactNode;
}

function inline(text: string, rules: Rule[]): ReactNode[] {
  const out: ReactNode[] = [];
  let rest = text;
  let key = 0;
  while (rest) {
    let best: { m: RegExpExecArray; r: Rule } | null = null;
    for (const r of rules) {
      const m = r.re.exec(rest);
      if (m && (!best || m.index < best.m.index)) best = { m, r };
    }
    if (!best) {
      out.push(rest);
      break;
    }
    if (best.m.index > 0) out.push(rest.slice(0, best.m.index));
    out.push(best.r.render(best.m, key++));
    rest = rest.slice(best.m.index + best.m[0].length);
  }
  return out;
}

function lines(text: string, render: (line: string) => ReactNode): ReactNode {
  return text.split("\n").map((l, i, all) => (
    <span key={i}>
      {render(l)}
      {i < all.length - 1 && <br />}
    </span>
  ));
}

const discordRules: Rule[] = [
  { re: /`([^`]+)`/, render: (m, k) => <code key={k}>{m[1]}</code> },
  { re: /\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/, render: (m, k) => <a key={k} href={m[2]} target="_blank" rel="noreferrer">{discord(m[1])}</a> },
  { re: /\*\*([\s\S]+?)\*\*/, render: (m, k) => <strong key={k}>{discord(m[1])}</strong> },
  { re: /__([\s\S]+?)__/, render: (m, k) => <u key={k}>{discord(m[1])}</u> },
  { re: /~~([\s\S]+?)~~/, render: (m, k) => <s key={k}>{discord(m[1])}</s> },
  { re: /(?:\*([^*\s][^*]*?)\*|_([^_\s][^_]*?)_)/, render: (m, k) => <em key={k}>{discord(m[1] ?? m[2])}</em> },
  { re: /<@&(\d+)>/, render: (m, k) => <span key={k} className="dc-mention">@role-{m[1].slice(-4)}</span> },
  { re: /<@!?(\d+)>/, render: (m, k) => <span key={k} className="dc-mention">@user-{m[1].slice(-4)}</span> },
  { re: /<#(\d+)>/, render: (m, k) => <span key={k} className="dc-mention">#channel-{m[1].slice(-4)}</span> },
  { re: /@(everyone|here)\b/, render: (m, k) => <span key={k} className="dc-mention">@{m[1]}</span> },
  { re: /https?:\/\/[^\s<>()]+/, render: (m, k) => <a key={k} href={m[0]} target="_blank" rel="noreferrer">{m[0]}</a> },
];

function discord(text: string): ReactNode {
  return inline(text, discordRules);
}

function discordBlock(text: string): ReactNode {
  return lines(text, (l) => {
    const h = /^(#{1,3}) (.*)$/.exec(l);
    if (h) return <span className={"dc-h" + h[1].length}>{discord(h[2])}</span>;
    const q = /^> (.*)$/.exec(l);
    if (q) return <span className="dc-quote">{discord(q[1])}</span>;
    return discord(l);
  });
}

const SLACK_EMOJI: Record<string, string> = {
  rotating_light: "🚨", fire: "🔥", warning: "⚠️", white_check_mark: "✅", heavy_check_mark: "✔️", x: "❌", robot_face: "🤖", bell: "🔔",
  red_circle: "🔴", large_green_circle: "🟢", green_circle: "🟢", large_blue_circle: "🔵", eyes: "👀", rocket: "🚀", tada: "🎉", bug: "🐛",
  sos: "🆘", boom: "💥", ambulance: "🚑", hammer_and_wrench: "🛠️", shield: "🛡️", satellite: "📡", chart_with_upwards_trend: "📈", new: "🆕",
  repeat: "🔁", test_tube: "🧪", zap: "⚡", skull: "💀", pager: "📟", siren: "🚨", mag: "🔍", owl: "🦉", lighthouse: "🗼",
};

const slackRules: Rule[] = [
  { re: /`([^`]+)`/, render: (m, k) => <code key={k}>{m[1]}</code> },
  { re: /<!date\^(\d+)\^[^|>]*\|([^>]*)>/, render: (m, k) => <span key={k}>{new Date(Number(m[1]) * 1000).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })}</span> },
  { re: /<!(channel|here|everyone)>/, render: (m, k) => <span key={k} className="sl-mention">@{m[1]}</span> },
  { re: /<!subteam\^[^|>]+\|([^>]+)>/, render: (m, k) => <span key={k} className="sl-mention">{m[1]}</span> },
  { re: /<@([A-Z0-9]+)(?:\|([^>]+))?>/, render: (m, k) => <span key={k} className="sl-mention">@{m[2] ?? m[1].toLowerCase()}</span> },
  { re: /<#([A-Z0-9]+)(?:\|([^>]+))?>/, render: (m, k) => <span key={k} className="sl-mention">#{m[2] ?? m[1].toLowerCase()}</span> },
  { re: /<(https?:\/\/[^|>]+)(?:\|([^>]+))?>/, render: (m, k) => <a key={k} href={m[1]} target="_blank" rel="noreferrer">{m[2] ? slack(m[2]) : m[1]}</a> },
  { re: /(?<![\w*])\*([^*\n]+)\*(?![\w*])/, render: (m, k) => <strong key={k}>{slack(m[1])}</strong> },
  { re: /(?<![\w_])_([^_\n]+)_(?![\w_])/, render: (m, k) => <em key={k}>{slack(m[1])}</em> },
  { re: /(?<![\w~])~([^~\n]+)~(?![\w~])/, render: (m, k) => <s key={k}>{slack(m[1])}</s> },
  { re: /:([a-z0-9_+-]+):/, render: (m, k) => <span key={k} title={m[0]}>{SLACK_EMOJI[m[1]] ?? m[0]}</span> },
];

function slack(text: string): ReactNode {
  return inline(text, slackRules);
}

function slackBlock(text: string): ReactNode {
  return lines(text, (l) => {
    const q = /^(?:>|&gt;) ?(.*)$/.exec(l);
    if (q) return <span className="sl-quote">{slack(q[1])}</span>;
    return slack(l);
  });
}

// ---- shared bits ----

function Avatar({ url, emoji, square }: { url?: string; emoji?: string; square?: boolean }) {
  const [broken, setBroken] = useState<string>();
  const cls = "wp-avatar" + (square ? " square" : "");
  if (emoji) return <div className={cls + " emoji"}>{SLACK_EMOJI[emoji.replace(/:/g, "")] ?? "🙂"}</div>;
  if (url && broken !== url) return <img className={cls} src={url} alt="" onError={() => setBroken(url)} />;
  return (
    <div className={cls + " logo"}>
      <Logo />
    </div>
  );
}

function clock(d = new Date()) {
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

const hex = (n: number) => "#" + n.toString(16).padStart(6, "0");

// ---- Discord ----

interface DiscordEmbed {
  title?: string;
  url?: string;
  description?: string;
  color?: number;
  timestamp?: string;
  fields?: { name: string; value: string; inline?: boolean }[];
  footer?: { text?: string };
}

interface DiscordPayload {
  username?: string;
  avatar_url?: string;
  content?: string;
  embeds?: DiscordEmbed[];
}

export function DiscordPreview({ payload }: { payload: DiscordPayload }) {
  return (
    <div className="wp discord">
      <div className="wp-msg">
        <Avatar url={payload.avatar_url} />
        <div className="wp-main">
          <div className="wp-head">
            <span className="wp-name">{payload.username || "Sentinel"}</span>
            <span className="wp-app">APP</span>
            <span className="wp-time">Today at {clock()}</span>
          </div>
          {payload.content && <div className="wp-text">{discordBlock(payload.content)}</div>}
          {payload.embeds?.map((e, i) => (
            <div key={i} className="dc-embed" style={{ borderLeftColor: e.color != null ? hex(e.color) : undefined }}>
              {e.title && (
                <div className="dc-title">
                  {e.url ? (
                    <a href={e.url} target="_blank" rel="noreferrer">
                      {discord(e.title)}
                    </a>
                  ) : (
                    discord(e.title)
                  )}
                </div>
              )}
              {e.description && <div className="dc-desc">{discordBlock(e.description)}</div>}
              {!!e.fields?.length && (
                <div className="dc-fields">
                  {e.fields.map((f, j) => (
                    <div key={j} className={"dc-field" + (f.inline ? "" : " full")}>
                      <div className="dc-fname">{discord(f.name)}</div>
                      <div className="dc-fvalue">{discordBlock(f.value)}</div>
                    </div>
                  ))}
                </div>
              )}
              {(e.footer?.text || e.timestamp) && (
                <div className="dc-footer">
                  {[e.footer?.text, e.timestamp && `Today at ${clock(new Date(e.timestamp))}`].filter(Boolean).join(" • ")}
                </div>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

// ---- Slack ----

interface SlackText {
  type: string;
  text: string;
}

interface SlackBlock {
  type: string;
  text?: SlackText;
  fields?: SlackText[];
  elements?: SlackText[];
}

interface SlackPayload {
  text?: string;
  username?: string;
  icon_emoji?: string;
  icon_url?: string;
  blocks?: SlackBlock[];
  attachments?: { color?: string; blocks?: SlackBlock[]; text?: string; fallback?: string }[];
}

function SlackBlocks({ blocks }: { blocks: SlackBlock[] }) {
  return (
    <>
      {blocks.map((b, i) => {
        if (b.type === "section")
          return (
            <div key={i} className="sl-section">
              {b.text && <div>{slackBlock(b.text.text)}</div>}
              {b.fields && (
                <div className="sl-fields">
                  {b.fields.map((f, j) => (
                    <div key={j}>{slackBlock(f.text)}</div>
                  ))}
                </div>
              )}
            </div>
          );
        if (b.type === "context")
          return (
            <div key={i} className="sl-context">
              {b.elements?.map((e, j) => (
                <span key={j}>{slack(e.text)}</span>
              ))}
            </div>
          );
        if (b.type === "divider") return <hr key={i} />;
        return null;
      })}
    </>
  );
}

export function SlackPreview({ payload }: { payload: SlackPayload }) {
  return (
    <div className="wp slack">
      <div className="wp-msg">
        <Avatar url={payload.icon_url} emoji={payload.icon_emoji} square />
        <div className="wp-main">
          <div className="wp-head">
            <span className="wp-name">{payload.username || "Sentinel"}</span>
            <span className="wp-app">APP</span>
            <span className="wp-time">{clock()}</span>
          </div>
          {payload.text && <div className="wp-text">{slackBlock(payload.text)}</div>}
          {payload.blocks && <SlackBlocks blocks={payload.blocks} />}
          {payload.attachments?.map((a, i) => (
            <div key={i} className="sl-attachment">
              <div className="sl-bar" style={{ background: a.color }} />
              <div className="sl-body">
                {a.text && <div>{slackBlock(a.text)}</div>}
                {a.blocks && <SlackBlocks blocks={a.blocks} />}
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
