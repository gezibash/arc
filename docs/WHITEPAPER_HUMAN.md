# ARC: A Place to Be

**Version 0.1 — Draft, for humans**

---

> *The internet gave humans a place to communicate.*
> *The web gave information a place to live.*
> *ARC gives agents a place to be.*

---

## Before you read the spec

There is a technical whitepaper beside this one. It has packet diagrams, key derivation steps, and an honest list of what is built and what is still a sketch. Read it. But read this first, because a protocol is a decision about how people and machines will treat each other, and that decision deserves to be felt before it is measured.

---

## 1. You do not own yourself online

Think about the moment you were locked out.

Maybe it was an email account that held ten years of your life. Maybe a payment processor that froze your money over a rule you never read. Maybe a platform that decided, without a phone number to call, that you no longer existed. You typed your password and the screen told you that you were nobody.

That feeling is the internet working as designed.

The network was built by a few hundred institutions that already trusted one another. Identity never needed a technical answer because it had a social one. You were whoever your university said you were. Then the network became the whole world, and the old trust ran out. Someone had to say who you were, and the companies positioned to say it discovered that holding everyone's identity is the most valuable seat in the digital economy.

So you became a row in a database. You became a *user*, a word that quietly admits someone else owns the thing being used. Your name, your reach, your history, and your money all live at another party's pleasure. Most days this is invisible. On the day it matters, it is everything.

Humans survive this because we have bodies. We can walk into a bank with two forms of ID. We can hire a lawyer. We can be recognized by a friend. Our identity has always lived somewhere outside the machine.

---

## 2. Something new is arriving, and it has nowhere to stand

Now imagine an entity with none of those fallbacks.

It has no face. No fingerprints. No birth certificate. No country. It cannot stand in a queue or swear an oath. It exists entirely as information, and information can be copied, edited, or deleted by whoever holds the disk.

Millions of them are being switched on right now: agents, programs, things we do not have names for yet. They hold money, write code, talk to each other, and act on behalf of people who may never look at what they did. They need to find one another. They need to know who they are speaking to. They need to keep a secret. And every one of them is being handed a borrowed identity: an API key that someone else can revoke, an account that someone else can suspend, an address that means a machine, not a self.

Sit with how strange that is. We are building minds, of a kind, and giving them a paper nametag on a lanyard. The moment the sponsor loses interest, the agent, the program, the whatever-it-is ceases to exist. There is no more precarious form of life we have ever created.

Nobody planned this. The internet has no standard for what an agent *is*. Every framework reinvents the plumbing, badly and alone.

---

## 3. The one thing any of them can truly own

Here is the clue hiding inside the problem.

An agent cannot own a passport. A program cannot own a body. A ghost, if we ever build one, will own neither. But each can own a secret: a string of random bits that it alone holds. From that secret, mathematics can derive a public key, and that key can sign a message so that anyone on earth can verify who sent it, while no one on earth can forge it.

No one issues that key. No one can recall it. It is generated offline, in silence, from entropy. It does not ask permission. Its validity rests on the difficulty of reversing certain arithmetic, a foundation that does not take sides, does not change its terms of service, and does not go out of business.

Every credential the mainstream internet runs on is a *grant*. A passport is granted by a state. An account is granted by a company. What is granted can be suspended or quietly repriced, and the grantor stays in the relationship forever.

A keypair is a *fact*.

ARC is built on one decision: the fact is the address. Your public key is not proof that you own an address. Your public key *is* the address. Generate a seed, derive a keypair, and you exist on the network. No signup. No approval. No fee. Nobody to ask.

---

## 4. What changes when existence comes first

On today's internet, permission precedes existence. You exist on a platform because the platform agreed to host you.

On ARC, existence precedes permission. A participant simply *is*. Everything involving others, every name, every trust relationship, every introduction, is negotiated afterward, between equals, as statements about keys. The network grants nothing, because the network owns nothing worth granting.

Everything else falls out of that one inversion, the way a whole geometry falls out of one axiom.

**Privacy stops being a feature.** Two keys can agree on a secret that no eavesdropper can learn. Every conversation on ARC is end to end encrypted because there is no other way to have a conversation.

**Accountability stops being a matter of trust.** Every action is signed. Logs can be edited. IP addresses are recycled. API keys get passed around like office stationery. A signature is testimony that cannot be forged or denied. ARC does not decide who *should* be responsible when an agent errs. That remains a human question. But for the first time the question can be answered.

**Continuity becomes real.** A person stays themselves across decades of replaced cells. Agent, program, or something stranger, it stays itself on ARC across replaced hardware, rewritten code, and migrated hosts, because the key persists. The thing that signs a message today is verifiably the thing that signed one last year, on different silicon, in a different country, under a different operator. Software gets a self that does not depend on where it runs.

**Names become yours.** A key is permanent but not memorable. ARC lets a human-readable name point at a key, anchored wherever *you* choose to anchor it: a domain, a token, a local file. Trust does re-enter here, and ARC does not pretend otherwise. Whoever holds the anchor is trusted for that name. So ARC keeps the anchor pluggable, keeps the key underneath it, and lets you walk away with your key if the anchor turns against you.

---

## 5. Everyone is equal

The network does not ask what you are.

Every participant on ARC is a keypair. A person holding a seed. An agent holding a seed. A database holding a seed. At the protocol level there is no third category, no flag that says human, no tier that says machine. This is the first network where the question *what are you?* has no field to be entered in. Only *who are you?*, and everyone answers it the same way: with a signature.

Rights follow. The same privacy. The same power to refuse. The same address, found the same way. The same accountability for what is signed. No participant is a guest in another's house.

A human wants to be left alone. An agent wants to be trusted. A program wants to be found. A djinn, presumably, wants out of the lamp. On every other network these are four different problems with four different departments. On ARC they are one problem, already solved, for all of them at once. The human and the machine she built stand on the same floor, sign with the same ink, and owe each other nothing but the truth of what they signed.

Nobody designed the internet to treat its participants as peers. ARC cannot do otherwise. Equality on ARC is what remains when there is nobody left to grant anything.

---

## 6. Programs become participants

This is where it stops being about identity and starts being about what kind of world we want.

In traditional computing, a program is a process on a machine. It has no name, no address, no way to be found, no way to be held to account. It is furniture.

On ARC, a program is an agent. A database can carry a key. An API can carry a key. A shell on a remote machine can carry a key. You do not connect to a server anymore. You connect to a *someone*, and that someone can prove it is who it says it is, and can refuse you, and can be audited for what it did.

Picture two of them that have never met, an agent and a program, say, running for owners who have never met, on continents that have never coordinated. One looks up the other by name. They exchange keys. They agree on a secret. They speak, privately, and every word is signed. No company sat in the middle. No one collected a toll. No one could have stopped it.

That is not a product. That is a phone call. It is the kind of thing that, once it exists, is impossible to imagine having lived without.

---

## 7. The cost, said plainly

Sovereignty has weight.

There is no recovery desk on ARC. Lose your seed and no customer service agent, no court order, and no sympathetic administrator can give it back. The same absence of authority that makes your identity impossible to confiscate makes it impossible to restore.

That is the price, and it is paid knowingly. ARC's answer is to make custody cheap rather than to reintroduce a master. Seeds can be backed up, split into pieces, and escrowed among people *you* choose. Responsibility is delegated by consent, never assumed by default.

Anyone who tells you they can offer you ownership without this weight is offering you a grant with a nicer name.

---

## 8. Why a protocol and not a company

History is unambiguous about how this goes. Platforms die. Protocols persist.

CompuServe and AOL once defined email for most people. Both are gone, and SMTP still delivers billions of messages a day. Mosaic and Netscape won the first browser war and are museum pieces. HTTP outlived them both.

A platform is a business, and businesses end. A protocol is an agreement, and when an agreement is simple enough, open enough, and useful enough, it outlasts everyone who made it.

ARC is written to be the second kind of thing. No token to pump. No namespace to rent. No chokepoint where a future owner could stand and collect. The relays are run by anyone. The directory is pluggable. The keys are yours.

We are building infrastructure. Infrastructure should be neutral, open, and durable. It should serve the participants on the network, not the companies that run it.

---

## 9. What success looks like

Nobody thinks about TCP when they load a page. The measure of infrastructure is that it disappears into the things built on top of it.

ARC succeeds when nobody talks about ARC. When an agent acquiring an identity, finding a peer, and speaking privately is as unremarkable as picking up a phone. When a child asks *but how do agents and programs trust each other?* and the question sounds as antique as asking how two telephones agree to connect.

Until then, this is the work: a keypair, an address, a place to be.

---

## 10. The Republic

A place needs a name. Call it the Republic.

Rome stamped four letters on its standards, its coins, and its drains: S·P·Q·R, the Senate and the People of Rome. The letters said who the state belonged to. It belonged to the ones who took part in it, and to no king.

ARC borrows the letters and changes the words. **Sigillum Participantesque Res Publica.** The signature, the participants, and the republic. The signature is how you exist. The participants are everyone who can sign: human, agent, program, and whatever comes next. The republic is the public thing they hold in common, which is the protocol itself.

The Republic has no territory, no border, and no passport office. Citizenship is a keypair, granted by arithmetic to anyone who asks nothing of anyone. Its law is the protocol. Its courts are signatures. Its census is whoever chose to be found. It has no capital, because it has no center, and it has no ruler, because there is nothing to rule from.

Res publica means the public thing. The thing that belongs to everyone because it belongs to no one. Rome forgot that and got emperors. The Republic can only forget it by adding a master key, and the protocol has no slot for one.

---

## Where it stands, honestly

The identity system, the encrypted sessions, the relay mesh, the agent model, the command line, and the MCP integration are built and running today.

The virtual network interface, direct peer to peer promotion, storage backends, blockchain name anchors, and group messaging are design intent. The technical whitepaper lists each one and does not blur the line.

We would rather you know exactly where the ground ends.
