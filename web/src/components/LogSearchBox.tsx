import { useEffect, useMemo, useRef, useState } from "react";
import { api, qs } from "../api";
import { recentSearches } from "../logsearch";
import { complete, FieldValue, renameField, Suggestion, unknownFields } from "../logsuggest";
import { Icon } from "./ui";

// The Logs search input with autocomplete: field names, then the values a
// field takes in recent logs, as you type.

export function LogSearchBox(props: {
  value: string;
  onChange: (v: string) => void;
  /** Runs a search (Enter with nothing picked, or a recent search). */
  onRun: (v: string) => void;
  fields: string[];
  services: string[];
}) {
  const { value, onChange, fields, services } = props;
  const input = useRef<HTMLInputElement>(null);
  const [cursor, setCursor] = useState(value.length);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const [values, setValues] = useState<{ field: string; prefix: string; values: FieldValue[] }>();
  const cache = useRef(new Map<string, FieldValue[]>());
  const recent = useMemo(() => (open ? recentSearches() : []), [open]);

  const c = useMemo(
    () => complete(value, Math.min(cursor, value.length), { fields, services, recent, values }),
    [value, cursor, fields, services, recent, values],
  );

  // Fetch values of the field being typed (debounced, cached per prefix).
  const want = c.valuesOf ? `${c.valuesOf.field}\u0000${c.valuesOf.prefix}` : "";
  useEffect(() => {
    if (!open || !c.valuesOf) return;
    const { field, prefix } = c.valuesOf;
    const hit = cache.current.get(want);
    if (hit) {
      setValues({ field, prefix, values: hit });
      return;
    }
    const t = setTimeout(() => {
      api.get<{ values: FieldValue[] }>("/api/logs/values" + qs({ field, prefix })).then(
        (r) => {
          cache.current.set(want, r.values);
          setValues((cur) => (cur && cur.field === field && cur.prefix.length > prefix.length ? cur : { field, prefix, values: r.values }));
        },
        () => undefined,
      );
    }, 150);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [want, open]);

  useEffect(() => setActive(-1), [c.from, c.items.length]);

  const items = open ? c.items : [];
  const sync = () => setCursor(input.current?.selectionStart ?? value.length);

  const accept = (s: Suggestion) => {
    let next = value.slice(0, c.from) + s.insert + value.slice(c.to);
    let pos = c.from + s.insert.length;
    if (s.insert.endsWith(" ") && next[pos] === " ") next = next.slice(0, pos) + next.slice(pos + 1);
    onChange(next);
    if (s.run) {
      setOpen(false);
      props.onRun(next);
      return;
    }
    setOpen(!!s.more || s.kind === "value" || s.kind === "keyword");
    requestAnimationFrame(() => {
      input.current?.focus();
      input.current?.setSelectionRange(pos, pos);
      setCursor(pos);
    });
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (!open) {
        setOpen(true);
        e.preventDefault();
        return;
      }
      if (!items.length) return;
      e.preventDefault();
      const d = e.key === "ArrowDown" ? 1 : -1;
      // -1 is "nothing picked" (Enter then searches); wraps around through it.
      const n = items.length + 1;
      setActive((a) => ((a + 1 + d + n) % n) - 1);
    } else if (e.key === "Tab" && items.length && !e.shiftKey) {
      e.preventDefault();
      accept(items[Math.max(active, 0)]);
    } else if (e.key === "Enter" && active >= 0 && items[active]) {
      e.preventDefault();
      accept(items[active]);
    } else if (e.key === "Enter") {
      setOpen(false);
    } else if (e.key === "Escape" && open) {
      e.preventDefault();
      setOpen(false);
    }
  };

  return (
    <div className="search">
      <Icon.search />
      <input
        ref={input}
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
          setCursor(e.target.selectionStart ?? e.target.value.length);
          setOpen(true);
        }}
        onKeyDown={onKeyDown}
        onKeyUp={(e) => (e.key === "ArrowLeft" || e.key === "ArrowRight" || e.key === "Home" || e.key === "End") && sync()}
        onClick={() => {
          sync();
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        placeholder="Search — e.g. level:error service:api timeout -healthcheck"
        aria-label="Search logs"
        aria-autocomplete="list"
        aria-expanded={items.length > 0}
        aria-controls="log-suggest"
        aria-activedescendant={active >= 0 ? `log-suggest-${active}` : undefined}
        role="combobox"
        spellCheck={false}
        autoComplete="off"
      />
      {items.length > 0 && (
        <div className="suggest" id="log-suggest" role="listbox">
          {items.map((s, i) => (
            <div
              key={s.kind + s.label}
              id={`log-suggest-${i}`}
              role="option"
              aria-selected={i === active}
              className={"suggest-row" + (i === active ? " on" : "")}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setActive(i)}
              onClick={() => accept(s)}
            >
              <span className={"suggest-kind k-" + s.kind}>{KIND[s.kind]}</span>
              <span className="mono suggest-label">{s.label}</span>
              {s.detail && <span className="small muted suggest-detail">{s.detail}</span>}
            </div>
          ))}
          <div className="suggest-foot small muted">
            <kbd>↑</kbd>
            <kbd>↓</kbd> choose · <kbd>Tab</kbd> complete · <kbd>Enter</kbd> search · <kbd>Esc</kbd> close
          </div>
        </div>
      )}
    </div>
  );
}

const KIND: Record<Suggestion["kind"], string> = { field: "field", value: "value", keyword: "op", recent: "recent" };

/**
 * Under the search box: what the typed search will match, in words, or why
 * it can't run, and fields no recent line has (with the likely intended one).
 */
export function LogSearchGuide(props: { draft: string; applied: string; fields: string[]; onFix: (q: string) => void }) {
  const { draft, applied, fields } = props;
  const [res, setRes] = useState<{ q: string; terms?: string[]; error?: string }>();
  useEffect(() => {
    if (!draft.trim()) {
      setRes(undefined);
      return;
    }
    const t = setTimeout(() => {
      api.get<{ terms?: string[]; error?: string }>("/api/logs/explain" + qs({ q: draft })).then(
        (r) => setRes({ q: draft, ...r }),
        () => undefined,
      );
    }, 250);
    return () => clearTimeout(t);
  }, [draft]);
  const hints = useMemo(() => unknownFields(draft, fields), [draft, fields]);
  if (!draft.trim() || !res || res.q !== draft) return null;
  const pending = draft.trim() !== applied.trim();
  return (
    <div className="search-guide small">
      {res.error ? (
        <span className="bad">⚠ {res.error.replace(/^search: /, "")}</span>
      ) : (
        res.terms &&
        res.terms.length > 0 && (
          <span className="search-guide-terms">
            <span className="muted">{pending ? "Will match lines where" : "Matching lines where"}</span>
            {res.terms.map((t, i) => (
              <span key={i}>
                {i > 0 && <span className="muted"> and </span>}
                <span className="search-term mono">{t}</span>
              </span>
            ))}
            {pending && <span className="faint"> · press Enter to search</span>}
          </span>
        )
      )}
      {hints.map((h) => (
        <span key={h.field} className="warn">
          No log in the last day has the field <code>{h.field}</code>
          {h.suggestion ? (
            <>
              {" "}
              — did you mean{" "}
              <button type="button" className="link mono" onClick={() => props.onFix(renameField(draft, h.field, h.suggestion!))}>
                {h.suggestion}
              </button>
              ?
            </>
          ) : (
            " (field names are case-sensitive)."
          )}
        </span>
      ))}
    </div>
  );
}
