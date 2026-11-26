# Amazon Photos Backup — design

## Intention

Sauvegarder sur un disque local l'intégralité d'une bibliothèque Amazon Photos
(amazon.fr), en continu et sans intervention : on s'authentifie une fois, puis
le service tourne en tâche de fond et survit aux redémarrages.

## Objectifs

- Télécharger les originaux (photos et vidéos) en pleine résolution.
- Incrémental : un fichier déjà sauvegardé n'est jamais retéléchargé.
- Une seule authentification manuelle ; la session est conservée sur disque et
  entretenue par le service.
- Livré comme une image Docker autonome (`docker compose up -d`).

## Hors périmètre

- macOS natif (abandonné à la demande de l'utilisateur) : Docker uniquement.
- Envoi vers Amazon, suppression côté Amazon, albums, synchronisation
  bidirectionnelle.
- Interface web.

## Anti-objectifs

- Supprimer ou écraser un fichier local parce qu'il a disparu ou changé côté
  Amazon. Une sauvegarde ne perd jamais de données.
- Envoyer les cookies de session ailleurs que sur `amazon.<tld>`.
- Échouer en silence quand la session a expiré.

## Contraintes

- Amazon Photos n'a pas d'API publique. On utilise l'API interne du site web
  (`https://www.amazon.fr/drive/v1`), authentifiée par les cookies du
  navigateur. Non documentée : peut casser sans préavis.
- La recherche est plafonnée : `offset + limit <= 9999`, 200 éléments par page.
- « Connecté pour toujours » ne peut pas être garanti : Amazon peut révoquer
  la session. Le service doit alors le signaler clairement et reprendre dès
  que de nouveaux cookies sont fournis.

## Approches envisagées

1. **Cookies du navigateur + API interne** (retenue). L'utilisateur colle une
   fois l'en-tête `Cookie` copié depuis son navigateur. Simple, pas de
   navigateur embarqué, image de quelques Mo.
2. **Navigateur headless dans le conteneur** qui se connecte avec
   identifiant/mot de passe. Écartée : captcha et 2FA d'Amazon rendent la
   connexion automatique fragile, il faudrait stocker le mot de passe, et
   l'image pèserait ~1 Go.
3. **Enregistrement en tant qu'appareil** (jeton de rafraîchissement façon
   appli mobile). Écartée : repose sur des identifiants d'application
   Amazon rétro-conçus, sans certitude que l'API Photos web accepte ces jetons.

## Design

Choix du constructeur (non décidés par l'utilisateur) : **Go, bibliothèque
standard uniquement**, binaire statique, image `distroless`.

**Commandes** : `run` (démon, par défaut), `login` (importe les cookies depuis
stdin ou un fichier, les valide, les enregistre), `once` (une passe),
`status`, `healthcheck`.

**Session** : les cookies sont stockés dans `/data/state/cookies.json`
(mode 0600). Un pot à cookies maison n'envoie les cookies qu'aux hôtes
`amazon.<tld>`, applique les `Set-Cookie` reçus et les réécrit sur disque.
Chaque cycle commence par une visite de `/photos/` pour entretenir la
session. Le démon relit le fichier s'il a été modifié par `login`.

**Inventaire** : `GET /drive/v1/search` avec le filtre
`type:(PHOTOS OR VIDEOS)`. Si le total dépasse 9999, découpage par
`timeYear`, puis `timeMonth`, puis `timeDay`. Le total des partitions est
comparé au total global ; tout écart est journalisé et complété par les 9999
éléments les plus récents.

**Téléchargement** : `GET /drive/v1/nodes/{id}/contentRedirection?querySuffix=?download=true&ownerId=…`,
écrit dans `<fichier>.part`, MD5 et taille vérifiés contre les métadonnées,
puis renommage atomique et date de modification = date de prise de vue.
4 téléchargements en parallèle par défaut, reprises avec backoff.

**Rangement** : `/data/photos/AAAA/MM/<nom d'origine>`. En cas de collision
de nom, suffixe `_<8 premiers caractères de l'id>`. Si le contenu d'un
élément change côté Amazon, la nouvelle version est ajoutée à côté de
l'ancienne.

**Index** : `/data/state/index.json` (id → chemin, md5, taille), écrit de
façon atomique. Si l'index est perdu, un fichier déjà présent avec le bon MD5
est réadopté sans retéléchargement.

**Session expirée** : détectée par HTTP 401/403, redirection vers
`/ap/signin` ou réponse HTML à la place du JSON. Le démon passe en état
`auth_required`, envoie une notification (webhook `NOTIFY_URL`, optionnel),
marque le conteneur `unhealthy`, et reprend dès que `login` a fourni de
nouveaux cookies.

**Configuration** (variables d'environnement) : `SYNC_INTERVAL` (6h),
`DOWNLOAD_CONCURRENCY` (4), `BACKUP_DIR`, `STATE_DIR`, `AMAZON_TLD`
(déduit des cookies), `USER_AGENT`, `NOTIFY_URL`.

## Vérification

Tests Go contre un faux serveur Amazon (pagination, plafond 9999,
redirection de téléchargement, rotation de cookies, expiration de session).
La validation contre le vrai service ne peut se faire que chez l'utilisateur.
