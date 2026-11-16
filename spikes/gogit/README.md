# آزمایش go-git (فاز ۰)

این پوشه یک ماژول Go ی جداست (`go.mod` خودش را دارد و جزو برنامه‌ی اصلی Karvan نیست). سؤالش این است: آیا
go-git می‌تواند بدون فرمان `git`، بدون worktree، بدون ادغام git و بدون force push، محل ذخیره‌ی git ی Karvan
باشد؟ یعنی fetch، ساختن commit مستقیم روی سر شاخه‌ی remote، push ی fast-forward، و اگر رد شد: fetch دوباره،
ساختن دوباره‌ی commit روی سر جدید و تلاش دوباره.

جواب: **بله**، هم روی repoهای محلی و هم روی GitHub از راه SSH ی پورت ۴۴۳، با چند نکته که پایین آمده و در `docs/sync-engine.md` (بخش ۱۲) هم لحاظ شده.

## فایل‌ها

| فایل | کار |
| ---- | ---- |
| `gitstore.go` | خود آزمایش: `Open`، `Fetch`، `ReadFile`، `List`، `Commit` (ساختن tree بدون checkout)، `Push` (فقط fast-forward، با شرط «remote هنوز همان‌جاست»)، `Update` (حلقه‌ی fetch ← ساختن ← push ← تلاش دوباره) |
| `gitstore_test.go` | تست‌ها روی repoهای bare ی محلی |
| `cmd/sshtry/` | فرمانی که شما روی سیستم خودتان با یک repo ی آزمایشی روی GitHub اجرا می‌کنید؛ `main_test.go` راه‌حل یافته‌ی ۶ را تست می‌کند |

## تست‌ها

```sh
cd spikes/gogit
go test -short ./...   # سریع (چند ثانیه)
go test ./...          # به‌علاوه‌ی اندازه‌گیری حافظه و زمان با ۶۴ مگابایت (حدود یک دقیقه)
```

هر سناریو دو بار اجرا می‌شود: یک بار با سرور داخلی خود go-git (هیچ فرمان `git` ی در کار نیست) و یک بار با
`git-receive-pack` ی سیستم (نزدیک‌ترین حالت به سرور واقعی مثل GitHub).

| تست | چه چیزی را نشان می‌دهد |
| ---- | ---- |
| `TestFirstPushToEmptyRemote` | اولین push به repo ی خالی |
| `TestRejectedPushThenRebuildAndRetry` | B اول push می‌کند، push ی A رد می‌شود، remote دست‌نخورده می‌ماند، A دوباره fetch می‌کند، commit را روی commit ی B می‌سازد و push موفق می‌شود؛ تاریخچه خطی است (هیچ merge commit ی نیست). پاک کردن همه‌ی فایل‌ها هم commit ی معتبر می‌سازد. |
| `TestUpdateRetriesUntilFastForward` | همین سناریو از راه حلقه‌ی `Update`؛ تلاش دوم فهرست B را می‌بیند |
| `TestConcurrentMachines` | پنج «سیستم» همزمان، هر کدام سه بار؛ همه‌ی نوشته‌ها می‌رسند، تاریخچه خطی است |
| `TestRewoundRemoteIsDetected` | اگر کسی شاخه را با force عقب ببرد، `Fetch` خطای `ErrRemoteRewound` می‌دهد و چیزی را «درست» نمی‌کند |
| `TestShallowClone` | با fetch ی کم‌عمق (depth 1) هم commit، رد شدن و تلاش دوباره کار می‌کند |
| `TestLargePushMemory` | زمان و حافظه برای push و fetch ی ۶۴ مگابایت داده‌ی غیرقابل فشرده‌سازی |

## یافته‌ها

- **نسخه:** go-git `v5.19.3` (آخرین نسخه‌ی v5 در ۱۴۰۵/۰۸/۲۵). این نسخه **Go 1.26** می‌خواهد؛ Go ی قدیمی‌تر
  (۱.۲۱ به بعد) با تنظیم پیش‌فرض `GOTOOLCHAIN=auto` خودش ۱.۲۶ را دانلود می‌کند.
- **بدون worktree و بدون فرمان git کار می‌کند.** commit با ساختن مستقیم tree روی tree ی سر remote ساخته می‌شود
  (`applyTree`)؛ `git fsck` ی واقعی هم نتیجه را سالم می‌داند. حافظه‌ی محلی می‌تواند روی دیسک (bare) یا کاملاً در
  RAM باشد.
- **رد شدن درست تشخیص داده می‌شود.** با `RequireRemoteRefs` (معادل compare-and-swap) خطای go-git این است:
  `remote ref refs/heads/main required to be <old> but is <new>`. وقتی دو push همزمان به سرور واقعی می‌رسند، یکی
  خطایی از خود سرور می‌گیرد که متنش به نسخه‌ی git بستگی دارد (یافته‌ی ۸)؛ این هم «رد شدن» حساب می‌شود و دوباره
  تلاش می‌شود.
- **یافته‌ی ۱ (مهم): fetch ی go-git به عقب رفتن شاخه بی‌اعتناست.** اگر objectها از قبل محلی باشند، go-git
  «already up to date» می‌گوید و ref ی ردیابی را جابه‌جا نمی‌کند، حتی وقتی شاخه‌ی remote با force عقب رفته. پس
  `Fetch` اول سر شاخه را مستقیم از remote می‌خواند (`ls-remote`)، بعد فقط چیزهای لازم را می‌گیرد و خودش ref را
  تنظیم می‌کند و بررسی می‌کند که سر قبلی جد سر جدید باشد.
- **یافته‌ی ۲:** اگر بعد از push ی ردشده، شاخه‌ی محلی روی commit ی push‌نشده بماند، fetch ی بعدی از سرور داخلی
  go-git با `object not found` می‌شکند. `Push` همیشه شاخه‌ی محلی را به سر remote برمی‌گرداند.
- **یافته‌ی ۳: سرعت.** جست‌وجوی delta ی go-git برای push ی ۶۴ مگابایت ~۲۹ ثانیه طول کشید. blobهای Karvan فشرده
  (و از M1 رمزنگاری‌شده) هستند و delta ندارند؛ با `pack.window = 0` همان push به ~۴ ثانیه رسید.
- **یافته‌ی ۴: حافظه.** اوج heap ی Go برای push ی ۶۴ مگابایت حدود ۲۳۰ تا ۲۸۰ مگابایت بود (go-git کل pack را در
  حافظه می‌سازد). پس Karvan بارگذاری بزرگ (مثلاً اولین همگام‌سازی) را در commitهای حداکثر ~۳۲ مگابایتی می‌فرستد.
- **یافته‌ی ۵:** سرور داخلی go-git از fetch ی کم‌عمق پشتیبانی نمی‌کند و ref را بدون قفل به‌روز می‌کند (دو push
  همزمان هر دو «موفق» می‌شوند و یکی گم می‌شود؛ بررسی عقب‌رفتن همین را گرفت). Karvan هیچ‌وقت سرور نیست، پس مهم
  نیست؛ این دو تست فقط با git ی واقعی اجرا می‌شوند.
- **یافته‌ی ۶ (مهم، از اجرای واقعی روی سیستم نگهدارنده): باگ go-git در بررسی کلید میزبان.** وقتی احراز هویت از
  ssh-agent یا فایل کلید است، go-git پایگاه `known_hosts` را برای بررسی کلید باز می‌کند ولی **نوع کلیدی** را که
  آن‌جا ثبت شده به سرور نمی‌گوید (کدی که این کار را می‌کند در v5.19.3 هرگز اجرا نمی‌شود). سرور نوع دیگری می‌فرستد و
  نتیجه `knownhosts: key mismatch` است، در حالی که `ssh -T` همان لحظه کار می‌کند. راه‌حل در `cmd/sshtry`
  (`pinHostKeys`): خودمان پایگاه را با `NewKnownHostsDb` باز می‌کنیم و هم callback و هم `HostKeyAlgorithms` را برای
  `host:port` تنظیم می‌کنیم. ورودی‌های hash‌شده‌ی `known_hosts` (پیش‌فرض Ubuntu) هم درست خوانده می‌شوند.
  محل ذخیره‌ی git ی Karvan و `doctor` باید همین کار را بکنند.
- **یافته‌ی ۸ (از CI ی GitHub با git 2.56): متن خطای رد شدن به نسخه‌ی git ی سرور بستگی دارد.** git ی جدید
  برخورد دو push همزمان را با `reference already exists` یا `incorrect old value provided` گزارش می‌کند، نه
  `failed to update ref` ی نسخه‌های قبلی. پس `Push` دیگر متن خطای سرور را تفسیر نمی‌کند: بعد از هر push ی ناموفق
  دوباره `ls-remote` می‌زند. اگر شاخه از جایی که انتظار داشتیم جلو رفته، یعنی رد شدن (تلاش دوباره)؛ اگر روی commit ی
  خود ماست، push نشسته؛ اگر عوض نشده، خطای واقعی است (مثلاً دسترسی).
- **یافته‌ی ۹ (از تست کامل فاز ۹): fetch ی کم‌عمق تکراری خراب می‌شود.** یک fetch ی depth 1 کار می‌کند (همان که
  `TestShallowClone` می‌آزماید)، ولی چند fetch ی depth 1 پشت سر هم در یک clone ی کم‌عمق، بعد از چند دور با
  `object not found` می‌شکند. محل ذخیره‌ی Karvan برای همین fetch ی کامل می‌زند؛ چون blobها کوچک‌اند و فقط
  تکه‌های تازه اضافه می‌شوند، هزینه‌اش کم است.
- **کمبودها که برای Karvan مهم نیستند:** go-git ادغام کامل و rebase ندارد. Karvan به هیچ‌کدام نیاز ندارد، چون
  هر سیستم فقط فایل‌هایی با اسم یکتای خودش اضافه می‌کند و commit را از نو روی سر remote می‌سازد.
- **نتیجه‌ی SSH ی واقعی (۱۴۰۵/۰۸/۲۵، سیستم Ubuntu ی نگهدارنده، `ssh://git@ssh.github.com:443/...`، repo ی
  خصوصی خالی):** همه‌ی ۸ مرحله پاس شد؛ push روی سر قدیمی با همان پیام `required to be ... but is ...` رد شد،
  تلاش دوباره در اولین دفعه موفق شد و هیچ force push ی انجام نشد. زمان‌ها:

  | مرحله | زمان |
  | ---- | ---- |
  | ۱. fetch (repo ی خالی) | ۴٫۲ ثانیه |
  | ۲. commit و push | ۳٫۰ ثانیه |
  | ۳. دو fetch | ۶٫۳ ثانیه |
  | ۴. push ی B | ۲٫۹ ثانیه |
  | ۵. push ی ردشده | ۲٫۱ ثانیه |
  | ۶. fetch، ساختن دوباره و push | ۷٫۴ ثانیه |
  | ۷. fetch در clone ی تازه | ۴٫۲ ثانیه |
  | ۸. پاک کردن | ۵٫۱ ثانیه |

  هر اتصال SSH روی آن شبکه حدود ۱ تا ۲ ثانیه است و `Fetch` دو اتصال می‌زند (`ls-remote` و fetch). پس یک دور
  همگام‌سازی معمولی (خواندن + push) حدود ۵ تا ۸ ثانیه شبکه می‌خواهد. برای زمان‌بند و `SessionEnd` مهم نیست، ولی
  hook ی `SessionStart` نباید آن‌قدر منتظر بماند (سؤال باز ۱۰ در `docs/sync-engine.md`).
- **یافته‌ی ۷: کلید از ssh-agent یا از فایل** هر دو کار کرد (`-key`). Karvan باید اجازه دهد کاربر کلید را مشخص کند
  و `doctor` بگوید کدام کلید استفاده شد.

## اجرا روی سیستم خودتان (SSH ی واقعی، پورت ۴۴۳)

> **فقط با یک repo ی آزمایشی دورریختنی.** هرگز با repo ی `claude-config` یا هیچ repo ی واقعی دیگری اجرا نکنید.
> فرمان خودش repo یی را که اسمش `test` ندارد رد می‌کند.

۱. روی GitHub یک repo ی **خصوصی** و خالی بسازید، مثلاً `karvan-test` (بدون README هم مشکلی نیست).

۲. Go نصب باشد (`go version`؛ اگر نیست: `sudo snap install go --classic` یا از go.dev/dl). نسخه‌ی ۱.۲۱ به بعد
کافی است؛ ۱.۲۶ خودکار دانلود می‌شود.

۳. یک بار کلید میزبان GitHub روی پورت ۴۴۳ را تأیید کنید (اگر قبلاً نکرده‌اید):

```sh
ssh -T -p 443 git@ssh.github.com
```

اثر انگشتی که نشان می‌دهد باید با اثر انگشت‌های منتشرشده‌ی GitHub (صفحه‌ی «GitHub's SSH key fingerprints» در
docs.github.com) یکی باشد؛ بعد `yes`. جواب درست این است: `Hi <user>! You've successfully authenticated...`

۴. اجرا (به‌جای `<owner>` نام کاربری GitHub خودتان):

```sh
git clone -b claude/festive-brahmagupta-ebfk2l https://github.com/leberman/Karvan.git /tmp/karvan-spike
cd /tmp/karvan-spike/spikes/gogit
go run ./cmd/sshtry -url ssh://git@ssh.github.com:443/<owner>/karvan-test.git
```

- کلید: اگر ssh-agent در حال اجراست از آن استفاده می‌کند، وگرنه `~/.ssh/id_ed25519` و بعد `~/.ssh/id_rsa`
  (اگر کلید عبارت عبور دارد، می‌پرسد). کلید دیگر: `-key ~/.ssh/<file>`.
- برای مقایسه با پورت ۲۲: `-url ssh://git@github.com/<owner>/karvan-test.git`. اگر شبکه پورت ۲۲ را می‌بندد،
  بعد از `-timeout` (پیش‌فرض ۶۰ ثانیه) پیام واضح می‌دهد.
- در پایان فایل‌هایش را با یک commit ی عادی پاک می‌کند (`-keep` برای نگه داشتن). commitها با اسم
  `Karvan (spike-a)` و ایمیل `karvan@invalid` ثبت می‌شوند؛ هیچ اطلاعات شخصی‌ای نه.

خروجی موفق شبیه این است:

```text
go-git v5.19.3, auth: ssh-agent
remote: ssh://git@ssh.github.com:443/<owner>/karvan-test.git (branch main)

ok   1. fetch (A)                                   ...
ok   2. commit + fast-forward push (A)              ...
ok   3. fetch (A and B see the same head)           ...
ok   4. B pushes first                              ...
     (rejected as expected: push rejected: remote branch moved (...))
ok   5. A pushes on the old head: must be REJECTED  ...
ok   6. fetch + rebuild on top + retry (A)          ...
     (1 attempt(s))
ok   7. fresh clone sees a1, b1 and a2              ...
ok   8. clean up (a normal commit deleting the files) ...

All steps passed. Nothing was force-pushed.
```

لطفاً کل خروجی (با زمان‌ها) را برای بررسی بفرستید؛ به‌خصوص اگر مرحله‌ی ۵ رد **نشد** یا متن خطایش متفاوت بود. بعد
می‌توانید repo ی `karvan-test` را پاک کنید.
