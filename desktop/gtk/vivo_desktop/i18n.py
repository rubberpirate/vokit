"""Small, dependency-free message catalogs shared by the GTK app and helper."""
import json
import locale
import os
from pathlib import Path

LANGUAGES = (("system", "Follow system"), ("en", "English"), ("zh_CN", "简体中文"),
             ("es", "Español"), ("fr", "Français"), ("de", "Deutsch"), ("ja", "日本語"))
SUPPORTED = {code for code, _name in LANGUAGES}
_language = "en"
_catalog = {}


def resolve_language(preference="system", environ=None):
    env = os.environ if environ is None else environ
    if preference in SUPPORTED and preference != "system":
        return preference
    candidates = []
    # Bare C/POSIX disables translated messages. C.UTF-8 still honors LANGUAGE,
    # which desktops use even when their process-wide locale is C.UTF-8.
    base = env.get("LC_ALL") or env.get("LC_MESSAGES") or env.get("LANG", "")
    if base in ("C", "POSIX"):
        return "en"
    candidates.extend(env.get("LANGUAGE", "").split(":"))
    candidates.append(base)
    if environ is None and not base:
        candidates.extend(locale.getlocale()[:1])
    for value in candidates:
        value = (value or "").split(".")[0].split("@")[0].replace("-", "_")
        if value in SUPPORTED and value != "system":
            return value
        primary = value.split("_")[0].lower()
        if primary == "zh":
            return "zh_CN"
        if primary in SUPPORTED and primary != "system":
            return primary
    return "en"


def configure(preference="system"):
    global _language, _catalog
    override = os.environ.get("VOKIT_LANGUAGE", "system")
    _language = resolve_language(override if override in SUPPORTED and override != "system" else preference)
    _catalog = {}
    if _language != "en":
        try:
            value = json.loads((Path(__file__).with_name("locales") / (_language + ".json")).read_text())
            if isinstance(value, dict):
                _catalog = value
        except (OSError, ValueError):
            pass  # English remains usable even if a translation is unavailable.
    return _language


def current_language():
    return _language


def gettext(message):
    return _catalog.get(message, message)


def N_(message):
    """Mark static tables; translate their entries when they are displayed."""
    return message


configure()
