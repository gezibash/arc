# Republic website engraving prompts

The original three images below were generated in **built-in ImageGen mode** for the ARC website. `docs/assets/arc-header.png` was inspected as a **style reference only**, never used as an edit target. The source images were then copied into this directory for project use.

## `prometheus-engraving.png`

```text
Use case: historical-scene
Asset type: republic.sh website hero artwork
Primary request: Original classical engraving of Prometheus in profile, raising a torch as an emblem of the freedom to participate.
Scene/backdrop: Distant Mediterranean mountain range, calm coastal haze, subtle engraved clouds.
Subject: Prometheus occupies the right third of a wide 3:2 landscape; an open, quiet parchment field fills the left half for site copy.
Style/medium: Meticulous nineteenth-century copperplate and woodcut engraving, very fine near-black ink crosshatching on warm light ivory parchment (#f3e7cf), matching a refined literary manifesto banner. Handmade paper grain, precise classical anatomy, elegant restrained linework.
Composition/framing: Prometheus faces left in sculptural profile with arm raised; torch flame and flowing cloak use restrained oxblood red accents. Keep all illustration safely within frame. No borders.
Lighting/mood: Dignified, luminous, quietly heroic.
Color palette: Ivory parchment, near-black engraved ink, sparse deep oxblood only in flame and cloak.
Constraints: no typography, no letters, no words, no logos, no watermark, no modern objects, no photorealism, preserve expansive empty left area.
```

## `sovereign-key-engraving.png`

```text
Use case: historical-scene
Asset type: republic.sh identity-section artwork
Primary request: Original antique sovereign key with a finely ornate bow and a modest round wax seal beside it.
Scene/backdrop: Warm light ivory parchment (#f3e7cf) with subtle handmade paper grain.
Subject: One precise old brass-style key rendered only as meticulous near-black copperplate engraving; a small plain wax seal with a subtle oxblood red accent, no legible markings.
Style/medium: Exquisite nineteenth-century intaglio engraving and archival bookplate illustration, fine crosshatching, restrained editorial elegance consistent with a classical manifesto.
Composition/framing: Portrait-oriented or square composition with generous surrounding empty parchment, central key on a slight diagonal, seal low beside it. Calm and uncluttered.
Lighting/mood: Scholarly, durable, self-sovereign.
Color palette: Ivory parchment, near-black ink, only sparse oxblood in the wax.
Constraints: no typography, no letters, no words, no initials, no logos, no watermark, no modern lock hardware, no photorealism.
```

## `republic-landscape-engraving.png`

```text
Use case: historical-scene
Asset type: republic.sh republic-section panoramic artwork
Primary request: Original sweeping Mediterranean coastal civic landscape, a republic held in common.
Scene/backdrop: Classical stone archways above a graceful small bridge, cypress trees, hilltop civic buildings and a broad calm sea with distant mountains.
Style/medium: Meticulous antique copperplate engraving and woodcut landscape in fine near-black ink, on warm light ivory parchment (#f3e7cf). Handmade paper grain, restrained literary editorial beauty, archival nineteenth-century travel-plate craft.
Composition/framing: Wide panoramic composition with connected civic architecture in the middle distance, open sky and sea, balanced detailed foreground and breathing room. No borders.
Lighting/mood: Calm, generous, enduring, sunlit without wash painting.
Color palette: Ivory parchment, near-black engraving, extremely sparse oxblood red accents only on a tiny bannerless rooftop tile or distant fabric, not prominent.
Constraints: no typography, no letters, no words, no flags, no logos, no watermark, no modern buildings, no cars, no people as focal subjects, no photorealism.
```

## Website delivery

The original PNG files are retained. The website uses WebP copies encoded with
`cwebp -q 85`, at the original dimensions, to reduce download size.


## Full illustrated edition

Additional chapter artwork uses the same built-in ImageGen workflow and the
existing header and Prometheus artwork as style references. Every scene is a
new illustration; the story text remains unchanged.

- [Origins and borrowed identity](PROMPTS-origins.md)
- [Identity and equality](PROMPTS-identity.md)
- [Participation and the republic](PROMPTS-republic.md)

Each prompt document records the exact scene prompts and the source dimensions.
The matching `.webp` files are the website delivery copies; `.png` files preserve
the generated originals.
