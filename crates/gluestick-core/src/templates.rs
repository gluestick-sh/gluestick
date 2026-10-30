//! Templates: named package bundles that live in a data root.
//!
//! A template is a curated set of packages with a **name**, so it can be
//! installed, updated or removed as one thing from the CLI
//! (`glue template install vibe-coding`) and from the desktop (the
//! Templates page, which shows the official ones).
//!
//! Where templates come from:
//!
//! * **Official** templates are the bundles compiled in from
//!   `assets/templates/*.bundle.json`; they are read-only and named after the
//!   bundle's template id (`vibe-coding`, `game-dev`, …).
//! * **User** templates are JSON files under `<data root>\templates\`. The
//!   file stem is always accepted as a name; a non-empty `name` field overrides
//!   it for display (the stem stays an alias, so renaming the field in the file
//!   cannot make the template unreachable). A user file that uses an official
//!   name shadows it, which is how a template is customized for one data root.
//!
//! Two file shapes are understood: `gluestick.template` (one template) and
//! `gluestick.template-bundle` (several templates — the shape the desktop's
//! "export as bundle" action writes), so a bundle exported by the GUI can be
//! dropped into the templates folder and used from the command line.
//!
//! Display text (`title`, `summary`, `description` and each package's `label`)
//! is carried by the file itself, so both frontends show the same words without
//! keeping their own copy. A value is either a plain string or a language map
//! (`{"en": …, "zh": …}`); see [`Localized`].

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::ser::Serializer;
use serde::{Deserialize, Serialize};

/// The official bundles ship inside this crate so both frontends read the same
/// template list.
const OFFICIAL_BUNDLES: &[&[u8]] = &[
    include_bytes!("../assets/templates/popular-downloads.bundle.json"),
    include_bytes!("../assets/templates/dev-environments.bundle.json"),
];

/// Template text that may be translated.
///
/// Files may write a plain string, which is the one form that renders in every
/// language, or a map of language code → text. A map is resolved by exact code,
/// then by language prefix (`zh-Hans` → `zh`), then by `en`, then by the first
/// non-empty value, so a missing translation never blanks a template.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(untagged)]
pub enum Localized {
    #[default]
    Empty,
    Text(String),
    Map(BTreeMap<String, String>),
}

/// An empty value is written as `""` rather than `null`, so a template file
/// saved by a frontend stays readable and parses back as [`Localized::Text`].
impl Serialize for Localized {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        match self {
            Localized::Empty => serializer.serialize_str(""),
            Localized::Text(text) => serializer.serialize_str(text),
            Localized::Map(map) => map.serialize(serializer),
        }
    }
}

impl Localized {
    pub fn is_empty(&self) -> bool {
        match self {
            Localized::Empty => true,
            Localized::Text(text) => text.trim().is_empty(),
            Localized::Map(map) => map.values().all(|value| value.trim().is_empty()),
        }
    }

    /// The text for `lang` (`"en"`, `"zh"`, `"zh-Hans"`, `"zh-CN"`, …). Unknown
    /// languages fall back to English and then to whatever the file declares.
    pub fn get(&self, lang: &str) -> &str {
        match self {
            Localized::Empty => "",
            Localized::Text(text) => text,
            Localized::Map(map) => {
                let language = primary_language(lang);
                let non_empty = |value: &&String| !value.trim().is_empty();
                map.get(lang)
                    .filter(non_empty)
                    .or_else(|| map.get(language).filter(non_empty))
                    .or_else(|| {
                        map.iter()
                            .find(|(key, value)| {
                                primary_language(key) == language && non_empty(value)
                            })
                            .map(|(_, value)| value)
                    })
                    .or_else(|| map.get("en").filter(non_empty))
                    .or_else(|| map.values().find(|value| non_empty(value)))
                    .map(String::as_str)
                    .unwrap_or("")
            }
        }
    }
}

fn primary_language(lang: &str) -> &str {
    lang.split(['-', '_']).next().unwrap_or(lang)
}

/// One package inside a template.
#[derive(Debug, Clone, Deserialize)]
pub struct TemplatePackage {
    pub name: String,
    /// Bucket the package comes from; empty or `main` means the main bucket.
    #[serde(default)]
    pub bucket: String,
    /// Display label, localized or a plain string; falls back to the package
    /// name.
    #[serde(default)]
    pub label: Localized,
}

impl TemplatePackage {
    /// The reference to hand to install/update: `bucket/name` for non-main
    /// buckets, bare `name` otherwise.
    pub fn install_ref(&self) -> String {
        if self.bucket.is_empty() || self.bucket == "main" {
            self.name.clone()
        } else {
            format!("{}/{}", self.bucket, self.name)
        }
    }

    pub fn bucket_label(&self) -> &str {
        if self.bucket.is_empty() {
            "main"
        } else {
            &self.bucket
        }
    }

    pub fn display(&self, lang: &str) -> &str {
        let label = self.label.get(lang);
        if label.is_empty() {
            &self.name
        } else {
            label
        }
    }
}

/// Where a template comes from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Origin {
    /// Compiled into the binary from `assets/templates`.
    Official,
    /// A JSON file in this data root.
    User,
}

impl Origin {
    pub fn as_str(&self) -> &'static str {
        match self {
            Origin::Official => "official",
            Origin::User => "user",
        }
    }
}

/// One template.
#[derive(Debug, Clone)]
pub struct Template {
    /// Name used on the command line (and as the desktop's template id).
    pub name: String,
    /// Other names that resolve to this template (the file stem).
    pub aliases: Vec<String>,
    pub origin: Origin,
    /// Heading shown for the template; falls back to `name`.
    pub title: Localized,
    /// One-line tagline shown under the title.
    pub summary: Localized,
    /// Long prose shown in the details pane.
    pub description: Localized,
    /// `ai` / `game` / `design` / `programming` / `popular`: the card's
    /// category filter. The label for each is frontend chrome.
    pub category: String,
    /// Emoji shown on the template card.
    pub icon: String,
    pub packages: Vec<TemplatePackage>,
    /// Source file of a user template.
    pub path: Option<PathBuf>,
}

impl Template {
    fn matches(&self, wanted: &str) -> bool {
        self.name.eq_ignore_ascii_case(wanted)
            || self.aliases.iter().any(|a| a.eq_ignore_ascii_case(wanted))
    }
}

/// Raw on-disk shape, shared by both file kinds: a `gluestick.template` fills
/// `packages`, a `gluestick.template-bundle` fills `templates`.
#[derive(Debug, Deserialize)]
struct RawFile {
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    title: Localized,
    #[serde(default)]
    summary: Localized,
    #[serde(default)]
    description: Localized,
    #[serde(default)]
    packages: Vec<TemplatePackage>,
    #[serde(default)]
    templates: Vec<RawTemplate>,
}

#[derive(Debug, Deserialize)]
struct RawTemplate {
    #[serde(default)]
    id: String,
    #[serde(default)]
    title: Localized,
    #[serde(default)]
    summary: Localized,
    #[serde(default)]
    description: Localized,
    #[serde(default)]
    category: String,
    #[serde(default)]
    icon: String,
    #[serde(default)]
    packages: Vec<TemplatePackage>,
}

/// `<data root>\templates` — where user templates live.
pub fn templates_dir(root: &Path) -> PathBuf {
    root.join("templates")
}

/// Every template visible in this data root, sorted by name. A user template
/// shadows an official one with the same name.
pub fn load_all(root: &Path) -> Vec<Template> {
    let mut templates = official();
    for user in user(root) {
        // A user file with an official name is that template, customized for this
        // data root — not a distinct template with an ambiguous name.
        if let Some(slot) = templates
            .iter_mut()
            .find(|r| r.origin == Origin::Official && r.name.eq_ignore_ascii_case(&user.name))
        {
            *slot = user;
        } else {
            templates.push(user);
        }
    }
    templates.sort_by_key(|r| r.name.to_lowercase());
    templates
}

/// Resolve a template by name (case-insensitive, file stem accepted).
pub fn find(root: &Path, wanted: &str) -> Result<Template, String> {
    let wanted = wanted.trim();
    if wanted.is_empty() {
        return Err("a template name is required".to_owned());
    }
    let all = load_all(root);
    let matches: Vec<&Template> = all.iter().filter(|r| r.matches(wanted)).collect();
    match matches.len() {
        1 => Ok(matches[0].clone()),
        0 => {
            let mut message = format!("template '{wanted}' not found");
            if all.is_empty() {
                message.push_str(&format!(
                    "\n  no templates yet; add a file to {}",
                    templates_dir(root).display()
                ));
            } else {
                let names: Vec<&str> = all.iter().map(|r| r.name.as_str()).collect();
                message.push_str(&format!("\n  available: {}", names.join(", ")));
            }
            Err(message)
        }
        _ => {
            let names: Vec<&str> = matches.iter().map(|r| r.name.as_str()).collect();
            Err(format!(
                "template name '{wanted}' is ambiguous (matches {})",
                names.join(", ")
            ))
        }
    }
}

/// The official templates, named after their template id.
pub fn official() -> Vec<Template> {
    let mut templates = Vec::new();
    for bytes in OFFICIAL_BUNDLES {
        let Ok(file) = serde_json::from_slice::<RawFile>(bytes) else {
            continue;
        };
        for template in file.templates {
            let name = template.id.trim();
            if name.is_empty() {
                continue;
            }
            templates.push(Template {
                name: name.to_owned(),
                aliases: Vec::new(),
                origin: Origin::Official,
                title: title_or_name(template.title, name),
                summary: template.summary,
                description: template.description,
                category: template.category,
                icon: template.icon,
                packages: normalize(template.packages),
                path: None,
            });
        }
    }
    templates
}

/// Templates read from this data root's templates folder.
fn user(root: &Path) -> Vec<Template> {
    let dir = templates_dir(root);
    let Ok(entries) = std::fs::read_dir(&dir) else {
        return Vec::new();
    };
    let mut templates = Vec::new();
    for entry in entries.flatten() {
        let path = entry.path();
        if !path
            .extension()
            .and_then(|e| e.to_str())
            .is_some_and(|e| e.eq_ignore_ascii_case("json"))
        {
            continue;
        }
        let stem = path
            .file_stem()
            .and_then(|s| s.to_str())
            .unwrap_or_default()
            .to_owned();
        let text = match std::fs::read_to_string(&path) {
            Ok(text) => text,
            Err(e) => {
                eprintln!("warning: {}: {e}", path.display());
                continue;
            }
        };
        // Editors on Windows (Notepad, PowerShell's `Set-Content -Encoding
        // utf8`) prepend a BOM; JSON parsers reject it.
        let text = text.strip_prefix('\u{feff}').unwrap_or(&text);
        let file: RawFile = match serde_json::from_str(text) {
            Ok(file) => file,
            Err(e) => {
                eprintln!("warning: {}: {e}", path.display());
                continue;
            }
        };
        let RawFile {
            name,
            title,
            summary,
            description,
            packages,
            templates: inner,
        } = file;
        if !inner.is_empty() {
            for template in inner {
                let id = template.id.trim();
                let name = if id.is_empty() {
                    stem.clone()
                } else {
                    id.to_owned()
                };
                let aliases = if id.is_empty() || id.eq_ignore_ascii_case(&stem) {
                    Vec::new()
                } else {
                    vec![stem.clone()]
                };
                // A bundle may still carry one description for all of its
                // templates; a per-template one wins.
                let description = if template.description.is_empty() {
                    description.clone()
                } else {
                    template.description
                };
                templates.push(Template {
                    title: title_or_name(template.title, &name),
                    name,
                    aliases,
                    origin: Origin::User,
                    summary: template.summary,
                    description,
                    category: template.category,
                    icon: template.icon,
                    packages: normalize(template.packages),
                    path: Some(path.clone()),
                });
            }
            continue;
        }
        let declared = name
            .as_deref()
            .map(str::trim)
            .filter(|name| !name.is_empty());
        let name = declared.unwrap_or(&stem).to_owned();
        let packages = normalize(packages);
        if packages.is_empty() {
            eprintln!("warning: {}: template has no packages", path.display());
        }
        templates.push(Template {
            name: name.clone(),
            aliases: match declared {
                Some(declared) if !declared.eq_ignore_ascii_case(&stem) => vec![stem],
                _ => Vec::new(),
            },
            origin: Origin::User,
            title: title_or_name(title, &name),
            summary,
            description,
            category: String::new(),
            icon: String::new(),
            packages,
            path: Some(path),
        });
    }
    templates
}

/// A template without its own title is shown under its name.
fn title_or_name(title: Localized, name: &str) -> Localized {
    if title.is_empty() {
        Localized::Text(name.to_owned())
    } else {
        title
    }
}

/// Labels default to the package name so a template never prints blanks.
fn normalize(packages: Vec<TemplatePackage>) -> Vec<TemplatePackage> {
    packages
        .into_iter()
        .map(|mut pkg| {
            if pkg.label.is_empty() {
                pkg.label = Localized::Text(pkg.name.clone());
            }
            pkg
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::{find, official, templates_dir, Localized, Origin};
    use std::path::PathBuf;

    /// A throwaway data root with an empty `templates` folder.
    fn root(tag: &str) -> PathBuf {
        let dir =
            std::env::temp_dir().join(format!("glue-template-test-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("templates")).unwrap();
        dir
    }

    #[test]
    fn official_bundles_parse_into_named_templates() {
        let templates = official();
        assert!(
            templates.len() >= 6,
            "{} official templates",
            templates.len()
        );
        assert!(templates
            .iter()
            .any(|template| template.name == "vibe-coding"));
        assert!(templates
            .iter()
            .all(|template| !template.packages.is_empty()));
        assert!(templates
            .iter()
            .all(|template| template.origin == Origin::Official));
        // The desktop renders the card from these two fields.
        assert!(templates
            .iter()
            .all(|template| !template.category.is_empty() && !template.icon.is_empty()));
    }

    /// Both frontends draw the text straight from the bundle, so every official
    /// template must carry it in both languages and every package must be
    /// labelled.
    #[test]
    fn official_text_is_localized_in_english_and_chinese() {
        let templates = official();
        let vibe = templates
            .iter()
            .find(|template| template.name == "vibe-coding")
            .unwrap();
        assert_eq!(vibe.title.get("en"), "Vibe Coding");
        assert_eq!(vibe.title.get("zh"), "氛围编程");
        assert_eq!(vibe.title.get("zh-Hans"), "氛围编程");
        assert!(!vibe.summary.get("zh").is_empty());
        assert!(!vibe.description.get("zh").is_empty());

        for template in &templates {
            for lang in ["en", "zh"] {
                assert!(
                    !template.title.get(lang).is_empty(),
                    "{}/title/{lang}",
                    template.name
                );
                assert!(
                    !template.summary.get(lang).is_empty(),
                    "{}/summary/{lang}",
                    template.name
                );
                assert!(
                    !template.description.get(lang).is_empty(),
                    "{}/description/{lang}",
                    template.name
                );
                for package in &template.packages {
                    assert!(
                        !package.display(lang).is_empty(),
                        "{}/{}/{lang}",
                        template.name,
                        package.name
                    );
                }
            }
        }
    }

    #[test]
    fn localized_text_accepts_a_plain_string_or_a_language_map() {
        let plain: Localized = serde_json::from_str(r#""Hello""#).unwrap();
        assert_eq!(plain.get("zh"), "Hello");

        let map: Localized = serde_json::from_str(r#"{"en":"Hi","zh":"你好"}"#).unwrap();
        assert_eq!(map.get("en"), "Hi");
        assert_eq!(map.get("zh"), "你好");
        assert_eq!(map.get("zh-CN"), "你好");
        // An unlisted language falls back to English.
        assert_eq!(map.get("fr"), "Hi");

        // A map without English falls back to whatever it does carry.
        let partial: Localized = serde_json::from_str(r#"{"zh":"你好"}"#).unwrap();
        assert_eq!(partial.get("en"), "你好");

        assert!(Localized::default().is_empty());
        assert_eq!(Localized::default().get("en"), "");
    }

    #[test]
    fn localized_serializes_as_text_or_map() {
        let text = Localized::Text("Hi".to_owned());
        assert_eq!(serde_json::to_string(&text).unwrap(), r#""Hi""#);

        // Empty text is `""`, never `null`: a file saved by a frontend must
        // parse back into the same shape.
        assert_eq!(
            serde_json::to_string(&Localized::default()).unwrap(),
            r#""""#
        );

        let map = Localized::Map([("en".to_owned(), "Hi".to_owned())].into_iter().collect());
        let json = serde_json::to_string(&map).unwrap();
        assert_eq!(json, r#"{"en":"Hi"}"#);
        assert_eq!(serde_json::from_str::<Localized>(&json).unwrap(), map);
    }

    #[test]
    fn user_template_resolves_by_name_and_by_file_stem() {
        let root = root("user");
        std::fs::write(
            templates_dir(&root).join("claude-code-template.json"),
            r#"{"schemaVersion":1,"kind":"gluestick.template","name":"Claude Code","packages":[
                 {"name":"claude-code","bucket":"main"},
                 {"name":"nodejs-lts"}]}"#,
        )
        .unwrap();

        let found = find(&root, "claude code").unwrap();
        assert_eq!(found.origin, Origin::User);
        assert_eq!(found.packages.len(), 2);
        assert_eq!(found.packages[1].install_ref(), "nodejs-lts");
        // Without display text, the name is the title and the package names are
        // the labels.
        assert_eq!(found.title.get("en"), "Claude Code");
        assert_eq!(found.packages[1].display("en"), "nodejs-lts");

        // The file stem is an alias, so renaming the display name in the file
        // does not make the template unreachable.
        assert_eq!(
            find(&root, "claude-code-template").unwrap().name,
            "Claude Code"
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_user_template_can_localize_its_text_and_labels() {
        let root = root("localized");
        std::fs::write(
            templates_dir(&root).join("editor.json"),
            r#"{"kind":"gluestick.template",
                "name":"Editor",
                "title":{"en":"Editor","zh":"编辑器"},
                "summary":{"en":"Code editors","zh":"代码编辑器"},
                "description":{"en":"Code editors.","zh":"代码编辑器。"},
                "packages":[{"name":"vscode","bucket":"extras","label":{"en":"VS Code","zh":"VS Code 编辑器"}}]}"#,
        )
        .unwrap();

        let found = find(&root, "editor").unwrap();
        assert_eq!(found.title.get("en"), "Editor");
        assert_eq!(found.title.get("zh"), "编辑器");
        assert_eq!(found.summary.get("zh"), "代码编辑器");
        assert_eq!(found.description.get("en"), "Code editors.");
        assert_eq!(found.packages[0].display("zh"), "VS Code 编辑器");
        assert_eq!(found.packages[0].display("fr"), "VS Code");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_user_file_shadows_the_official_template_of_the_same_name() {
        let root = root("shadow");
        std::fs::write(
            templates_dir(&root).join("vibe-coding.json"),
            r#"{"kind":"gluestick.template","packages":[{"name":"claude-code"}]}"#,
        )
        .unwrap();

        let found = find(&root, "vibe-coding").unwrap();
        assert_eq!(found.origin, Origin::User);
        assert_eq!(found.packages.len(), 1);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn a_utf8_bom_does_not_hide_the_template() {
        let root = root("bom");
        std::fs::write(
            templates_dir(&root).join("bom-template.json"),
            "\u{feff}{\"kind\":\"gluestick.template\",\"packages\":[{\"name\":\"7zip\"}]}",
        )
        .unwrap();

        let found = find(&root, "bom-template").unwrap();
        assert_eq!(found.packages.len(), 1);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn unknown_names_list_what_is_available() {
        let root = root("missing");
        let message = find(&root, "nope").unwrap_err();
        assert!(message.contains("not found"), "{message}");
        assert!(message.contains("vibe-coding"), "{message}");
        let _ = std::fs::remove_dir_all(&root);
    }
}
