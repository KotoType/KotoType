// XVoice text service: a TSF speech-category TIP that TSF activates next to
// whatever keyboard IME is in use. It does nothing until xvoice.exe talks to
// it: for each thread that gets a text focus it creates a message-only window
// ("XVoiceTipWindow", window text = protocol version) that accepts
// WM_COPYDATA commands on that UI thread.
//
// It runs inside every GUI process for as long as they live, so it is kept to
// the bare mechanism: all policy is in xvoice.exe, nothing here blocks, every
// edit is queued asynchronously, and there is no C runtime (no per-process
// runtime start-up, heap or thread storage; memory comes from the process
// heap). Old copies stay loaded in long-running processes after an update, so
// the protocol only ever grows.
//
// Protocol 1 (COPYDATASTRUCT.dwData, lpData = UTF-16 text without terminator):
//   QUERY    -> (generation << 2) | state; state 1 editable, 2 not writable
//               (read-only, or the application disabled keyboard input as it
//               does for passwords), 0 when the thread has no text focus.
//               generation changes whenever the focus moves or someone else
//               ends our composition.
//   COMPOSE  -> show text as the composition, starting one if needed.
//   COMMIT   -> replace the composition with text (if any) and end it.
//   CARET    -> read screen rect of selection (returns 1) or view (returns 2),
//               or 0 if unavailable; cached for CARET_LT / CARET_RB.
//   CARET_LT -> packed int16 (left | top<<16) from the last CARET call.
//   CARET_RB -> packed int16 (right | bottom<<16) from the last CARET call.
// COMPOSE and COMMIT return 1 when the edit was queued.
//
// WM_COPYDATA from lower-integrity processes stays blocked (UIPI): letting it
// through would let any program type into elevated windows.
#include <windows.h>
#include <msctf.h>
#include <olectl.h>
#include <new>
#include "tip_id.h"

namespace {

const CLSID CLSID_KotoTypeTip = KT_CLSID_INIT;
const GUID PROFILE_KotoTypeTip = KT_PROFILE_INIT;
const wchar_t DESCRIPTION[] = L"KotoType";
const wchar_t WINDOW_CLASS[] = KT_WINDOW_CLASS;
const wchar_t PROTOCOL[] = L"2";
const DWORD MAX_TEXT_BYTES = 1 << 20; // kototype.exe sends one dictation at most.
const LANGID LANGUAGES[] = {0x0804, 0x0409}; // zh-CN, en-US
// Addresses, not copies: copying the extern GUIDs would need a run-time
// initializer, and there is no C runtime to run it.
const GUID *const CATEGORIES[] = {&GUID_TFCAT_TIP_SPEECH, &GUID_TFCAT_TIPCAP_IMMERSIVESUPPORT, &GUID_TFCAT_TIPCAP_SYSTRAYSUPPORT};
const wchar_t CLSID_KEY[] = L"Software\\Classes\\CLSID\\" KT_CLSID_STR;
const wchar_t TIP_KEY[] = L"Software\\Microsoft\\CTF\\TIP\\" KT_CLSID_STR;

enum Command : ULONG_PTR { QUERY = 1, COMPOSE = 2, COMMIT = 3, CARET = 4, CARET_LT = 5, CARET_RB = 6 };

HINSTANCE dllInstance;

// Objects live on the process heap; a failed allocation yields nullptr.
template <class T, class... Args> T *Create(Args... args) {
    void *memory = HeapAlloc(GetProcessHeap(), 0, sizeof(T));
    return memory ? new (memory) T(args...) : nullptr;
}

template <class T> void Destroy(T *object) {
    object->~T();
    HeapFree(GetProcessHeap(), 0, object);
}

class TextService;

// One queued edit: COMPOSE or COMMIT of text into the service's context. It
// owns a copy of the text.
class EditSession final : public ITfEditSession {
public:
    EditSession(TextService *service, ITfContext *context, Command command, wchar_t *text, LONG length);
    ~EditSession();
    STDMETHODIMP QueryInterface(REFIID riid, void **ppv) override {
        if (riid == IID_IUnknown || riid == IID_ITfEditSession) {
            *ppv = static_cast<ITfEditSession *>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&refs);
        if (!r) Destroy(this);
        return r;
    }
    STDMETHODIMP DoEditSession(TfEditCookie ec) override;

private:
    LONG refs = 1;
    TextService *service;
    ITfContext *context;
    Command command;
    wchar_t *text;
    LONG length;
};

// Synchronous read-only edit session used to query the caret/view rectangle.
class CaretSession final : public ITfEditSession {
public:
    CaretSession(TextService *service, ITfContext *context);
    ~CaretSession();
    STDMETHODIMP QueryInterface(REFIID riid, void **ppv) override {
        if (riid == IID_IUnknown || riid == IID_ITfEditSession) {
            *ppv = static_cast<ITfEditSession *>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&refs);
        if (!r) Destroy(this);
        return r;
    }
    STDMETHODIMP DoEditSession(TfEditCookie ec) override;

private:
    LONG refs = 1;
    TextService *service;
    ITfContext *context;
};

class TextService final : public ITfTextInputProcessorEx, public ITfThreadMgrEventSink, public ITfCompositionSink {
public:
    ~TextService() {
        if (window) DestroyWindow(window);
        DropComposition();
    }

    STDMETHODIMP QueryInterface(REFIID riid, void **ppv) override {
        if (riid == IID_IUnknown || riid == IID_ITfTextInputProcessor || riid == IID_ITfTextInputProcessorEx) {
            *ppv = static_cast<ITfTextInputProcessorEx *>(this);
        } else if (riid == IID_ITfThreadMgrEventSink) {
            *ppv = static_cast<ITfThreadMgrEventSink *>(this);
        } else if (riid == IID_ITfCompositionSink) {
            *ppv = static_cast<ITfCompositionSink *>(this);
        } else {
            *ppv = nullptr;
            return E_NOINTERFACE;
        }
        AddRef();
        return S_OK;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&refs);
        if (!r) Destroy(this);
        return r;
    }

    STDMETHODIMP Activate(ITfThreadMgr *threadMgr, TfClientId id) override { return ActivateEx(threadMgr, id, 0); }
    STDMETHODIMP ActivateEx(ITfThreadMgr *threadMgr, TfClientId id, DWORD) override {
        manager = threadMgr;
        manager->AddRef();
        client = id;
        ITfSource *source = nullptr;
        if (SUCCEEDED(manager->QueryInterface(IID_ITfSource, (void **)&source))) {
            source->AdviseSink(IID_ITfThreadMgrEventSink, static_cast<ITfThreadMgrEventSink *>(this), &sinkCookie);
            source->Release();
        }
        ITfDocumentMgr *focus = nullptr;
        if (SUCCEEDED(manager->GetFocus(&focus)) && focus) {
            EnsureWindow();
            focus->Release();
        }
        return S_OK;
    }
    STDMETHODIMP Deactivate() override {
        if (window) {
            DestroyWindow(window);
            window = nullptr;
        }
        DropComposition();
        if (manager) {
            ITfSource *source = nullptr;
            if (sinkCookie != TF_INVALID_COOKIE && SUCCEEDED(manager->QueryInterface(IID_ITfSource, (void **)&source))) {
                source->UnadviseSink(sinkCookie);
                source->Release();
            }
            sinkCookie = TF_INVALID_COOKIE;
            manager->Release();
            manager = nullptr;
        }
        return S_OK;
    }

    STDMETHODIMP OnInitDocumentMgr(ITfDocumentMgr *) override { return S_OK; }
    STDMETHODIMP OnUninitDocumentMgr(ITfDocumentMgr *) override { return S_OK; }
    STDMETHODIMP OnSetFocus(ITfDocumentMgr *focus, ITfDocumentMgr *) override {
        generation++;
        if (focus) EnsureWindow();
        return S_OK;
    }
    STDMETHODIMP OnPushContext(ITfContext *) override { return S_OK; }
    STDMETHODIMP OnPopContext(ITfContext *) override { return S_OK; }

    // The application or another text service ended our composition.
    STDMETHODIMP OnCompositionTerminated(TfEditCookie, ITfComposition *ended) override {
        if (ended == composition) {
            DropComposition();
            generation++;
        }
        return S_OK;
    }

    // Edit session helpers, only called inside DoEditSession.
    HRESULT Compose(TfEditCookie ec, ITfContext *context, const wchar_t *text, LONG length) {
        if (composition && composedContext != context) EndComposition(ec);
        if (!composition) {
            TF_SELECTION selection = {};
            ULONG fetched = 0;
            HRESULT hr = context->GetSelection(ec, TF_DEFAULT_SELECTION, 1, &selection, &fetched);
            if (FAILED(hr) || !fetched || !selection.range) return FAILED(hr) ? hr : E_FAIL;
            ITfContextComposition *composer = nullptr;
            hr = context->QueryInterface(IID_ITfContextComposition, (void **)&composer);
            if (SUCCEEDED(hr)) {
                hr = composer->StartComposition(ec, selection.range, static_cast<ITfCompositionSink *>(this), &composition);
                composer->Release();
            }
            selection.range->Release();
            if (FAILED(hr) || !composition) {
                composition = nullptr;
                return FAILED(hr) ? hr : E_FAIL;
            }
            composedContext = context;
            composedContext->AddRef();
        }
        ITfRange *range = nullptr;
        HRESULT hr = composition->GetRange(&range);
        if (FAILED(hr)) return hr;
        hr = range->SetText(ec, 0, text, length);
        if (SUCCEEDED(hr)) {
            // Keep the caret after the composed text.
            range->Collapse(ec, TF_ANCHOR_END);
            TF_SELECTION caret = {range, {TF_AE_NONE, FALSE}};
            context->SetSelection(ec, 1, &caret);
        }
        range->Release();
        return hr;
    }
    HRESULT Commit(TfEditCookie ec, ITfContext *context, const wchar_t *text, LONG length) {
        if (length) {
            HRESULT hr = Compose(ec, context, text, length);
            if (FAILED(hr)) return hr;
        }
        EndComposition(ec);
        return S_OK;
    }
    void ReadCaret(TfEditCookie ec, ITfContext *context) {
        caretRect = {};
        caretKind = 0;
        ITfContextView *view = nullptr;
        if (FAILED(context->GetActiveView(&view)) || !view) return;
        TF_SELECTION selection = {};
        ULONG fetched = 0;
        if (SUCCEEDED(context->GetSelection(ec, TF_DEFAULT_SELECTION, 1, &selection, &fetched)) && fetched && selection.range) {
            RECT rc = {};
            BOOL clipped = FALSE;
            if (SUCCEEDED(view->GetTextExt(ec, selection.range, &rc, &clipped)) &&
                (rc.right > rc.left || rc.bottom > rc.top)) {
                caretRect = rc;
                caretKind = 1;
            }
            selection.range->Release();
        }
        if (!caretKind) {
            RECT rc = {};
            if (SUCCEEDED(view->GetScreenExt(&rc)) && rc.right > rc.left && rc.bottom > rc.top) {
                caretRect = rc;
                caretKind = 2;
            }
        }
        view->Release();
    }

private:
    LONG refs = 1;
    ITfThreadMgr *manager = nullptr;
    TfClientId client = TF_CLIENTID_NULL;
    DWORD sinkCookie = TF_INVALID_COOKIE;
    HWND window = nullptr;
    ITfComposition *composition = nullptr;
    ITfContext *composedContext = nullptr;
    ULONG generation = 1;
    RECT caretRect = {};
    int caretKind = 0;

    ITfContext *FocusedContext() {
        ITfDocumentMgr *focus = nullptr;
        if (!manager || FAILED(manager->GetFocus(&focus)) || !focus) return nullptr;
        ITfContext *context = nullptr;
        focus->GetTop(&context);
        focus->Release();
        return context;
    }

    // Flagged reads a context compartment the application sets, such as
    // keyboard-disabled on password fields.
    static bool Flagged(ITfContext *context, REFGUID compartment) {
        ITfCompartmentMgr *compartments = nullptr;
        if (FAILED(context->QueryInterface(IID_ITfCompartmentMgr, (void **)&compartments))) return false;
        ITfCompartment *item = nullptr;
        VARIANT value;
        VariantInit(&value);
        if (SUCCEEDED(compartments->GetCompartment(compartment, &item))) {
            item->GetValue(&value);
            item->Release();
        }
        compartments->Release();
        bool set = value.vt == VT_I4 && value.lVal != 0;
        VariantClear(&value);
        return set;
    }

    void EndComposition(TfEditCookie ec) {
        if (composition) composition->EndComposition(ec);
        DropComposition();
    }
    void DropComposition() {
        if (composition) {
            composition->Release();
            composition = nullptr;
        }
        if (composedContext) {
            composedContext->Release();
            composedContext = nullptr;
        }
    }

    // The window is created on the first text focus so that threads that
    // never edit text (most of them) carry nothing.
    void EnsureWindow() {
        if (window) return;
        WNDCLASSEXW wc = {sizeof(wc)};
        wc.lpfnWndProc = WindowProc;
        wc.hInstance = dllInstance;
        wc.lpszClassName = WINDOW_CLASS;
        RegisterClassExW(&wc); // Fails harmlessly once registered.
        window = CreateWindowExW(0, WINDOW_CLASS, PROTOCOL, 0, 0, 0, 0, 0, HWND_MESSAGE, nullptr, dllInstance, nullptr);
        if (window) SetWindowLongPtrW(window, GWLP_USERDATA, (LONG_PTR)this);
    }

    LRESULT Handle(ULONG_PTR command, const wchar_t *text, LONG length) {
        if (command == CARET_LT) {
            return (LRESULT)((DWORD)(USHORT)(SHORT)caretRect.left | ((DWORD)(USHORT)(SHORT)caretRect.top << 16));
        }
        if (command == CARET_RB) {
            return (LRESULT)((DWORD)(USHORT)(SHORT)caretRect.right | ((DWORD)(USHORT)(SHORT)caretRect.bottom << 16));
        }
        ITfContext *context = FocusedContext();
        if (!context) return 0;
        LRESULT answer = 0;
        if (command == QUERY) {
            TF_STATUS status = {};
            context->GetStatus(&status);
            bool writable = !(status.dwDynamicFlags & TF_SD_READONLY) && !Flagged(context, GUID_COMPARTMENT_KEYBOARD_DISABLED) &&
                            !Flagged(context, GUID_COMPARTMENT_EMPTYCONTEXT);
            answer = ((LRESULT)(generation & 0x1fffffff) << 2) | (writable ? 1 : 2);
        } else if (command == CARET) {
            caretRect = {};
            caretKind = 0;
            CaretSession *session = Create<CaretSession>(this, context);
            if (session) {
                HRESULT result = S_OK;
                context->RequestEditSession(client, session, TF_ES_SYNC | TF_ES_READ, &result);
                session->Release();
            }
            answer = caretKind;
        } else if (command == COMPOSE || command == COMMIT) {
            // The text belongs to the sender; the queued edit keeps a copy.
            wchar_t *copy = length ? (wchar_t *)HeapAlloc(GetProcessHeap(), 0, length * sizeof(wchar_t)) : nullptr;
            EditSession *session = (copy || !length) ? Create<EditSession>(this, context, (Command)command, copy, length) : nullptr;
            if (session) {
                if (copy) CopyMemory(copy, text, length * sizeof(wchar_t));
                HRESULT result = S_OK;
                // Always queued: never edit inside a message another process waits on.
                answer = SUCCEEDED(context->RequestEditSession(client, session, TF_ES_ASYNC | TF_ES_READWRITE, &result)) ? 1 : 0;
                session->Release();
            } else if (copy) {
                HeapFree(GetProcessHeap(), 0, copy);
            }
        }
        context->Release();
        return answer;
    }

    static LRESULT CALLBACK WindowProc(HWND hwnd, UINT message, WPARAM wParam, LPARAM lParam) {
        if (message == WM_COPYDATA) {
            auto *service = (TextService *)GetWindowLongPtrW(hwnd, GWLP_USERDATA);
            auto *data = (const COPYDATASTRUCT *)lParam;
            if (!service || !data || data->cbData > MAX_TEXT_BYTES) return 0;
            LONG length = data->lpData ? (LONG)(data->cbData / sizeof(wchar_t)) : 0;
            return service->Handle(data->dwData, (const wchar_t *)data->lpData, length);
        }
        if (message == WM_CLOSE) {
            if (auto *service = (TextService *)GetWindowLongPtrW(hwnd, GWLP_USERDATA)) {
                SetWindowLongPtrW(hwnd, GWLP_USERDATA, 0);
                service->Deactivate();
                return 0;
            }
        }
        return DefWindowProcW(hwnd, message, wParam, lParam);
    }
};

EditSession::EditSession(TextService *s, ITfContext *c, Command cmd, wchar_t *t, LONG n)
    : service(s), context(c), command(cmd), text(t), length(n) {
    service->AddRef();
    context->AddRef();
}

EditSession::~EditSession() {
    if (text) HeapFree(GetProcessHeap(), 0, text);
    context->Release();
    service->Release();
}

STDMETHODIMP EditSession::DoEditSession(TfEditCookie ec) {
    return command == COMPOSE ? service->Compose(ec, context, text, length) : service->Commit(ec, context, text, length);
}

CaretSession::CaretSession(TextService *s, ITfContext *c) : service(s), context(c) {
    service->AddRef();
    context->AddRef();
}

CaretSession::~CaretSession() {
    context->Release();
    service->Release();
}

STDMETHODIMP CaretSession::DoEditSession(TfEditCookie ec) {
    service->ReadCaret(ec, context);
    return S_OK;
}

class Factory final : public IClassFactory {
public:
    STDMETHODIMP QueryInterface(REFIID riid, void **ppv) override {
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory *>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }
    STDMETHODIMP_(ULONG) AddRef() override { return InterlockedIncrement(&refs); }
    STDMETHODIMP_(ULONG) Release() override {
        LONG r = InterlockedDecrement(&refs);
        if (!r) Destroy(this);
        return r;
    }
    STDMETHODIMP CreateInstance(IUnknown *outer, REFIID riid, void **ppv) override {
        *ppv = nullptr;
        if (outer) return CLASS_E_NOAGGREGATION;
        TextService *service = Create<TextService>();
        if (!service) return E_OUTOFMEMORY;
        HRESULT hr = service->QueryInterface(riid, ppv);
        service->Release();
        return hr;
    }
    STDMETHODIMP LockServer(BOOL) override { return S_OK; }

private:
    LONG refs = 1;
};

} // namespace

// The compiler may emit calls to these for struct initialisation, copies and
// GUID comparisons, and references operator delete from destructors.
extern "C" {
#pragma function(memset, memcpy, memcmp)
void *__cdecl memset(void *destination, int value, size_t size) {
    auto *p = (volatile unsigned char *)destination;
    while (size--) *p++ = (unsigned char)value;
    return destination;
}
void *__cdecl memcpy(void *destination, const void *source, size_t size) {
    auto *d = (volatile unsigned char *)destination;
    auto *s = (const unsigned char *)source;
    while (size--) *d++ = *s++;
    return destination;
}
int __cdecl memcmp(const void *a, const void *b, size_t size) {
    auto *x = (const volatile unsigned char *)a;
    auto *y = (const volatile unsigned char *)b;
    for (; size--; x++, y++) {
        if (*x != *y) return *x < *y ? -1 : 1;
    }
    return 0;
}
}

void operator delete(void *memory) noexcept { HeapFree(GetProcessHeap(), 0, memory); }
void operator delete(void *memory, size_t) noexcept { HeapFree(GetProcessHeap(), 0, memory); }

// Without the C runtime this is the DLL's entry point.
extern "C" BOOL WINAPI DllMain(HINSTANCE instance, DWORD reason, LPVOID) {
    if (reason == DLL_PROCESS_ATTACH) {
        dllInstance = instance;
        DisableThreadLibraryCalls(instance);
    }
    return TRUE;
}

STDAPI DllGetClassObject(REFCLSID clsid, REFIID riid, void **ppv) {
    *ppv = nullptr;
    if (clsid != CLSID_KotoTypeTip) return CLASS_E_CLASSNOTAVAILABLE;
    Factory *factory = Create<Factory>();
    if (!factory) return E_OUTOFMEMORY;
    HRESULT hr = factory->QueryInterface(riid, ppv);
    factory->Release();
    return hr;
}

// Windows and edit sessions point into this DLL; it stays until the process exits.
STDAPI DllCanUnloadNow() { return S_FALSE; }

STDAPI DllUnregisterServer();

// Machine-wide registration (regsvr32, elevated): the COM class, one profile
// per language and the speech category that makes TSF activate it alongside
// the keyboard IME.
STDAPI DllRegisterServer() {
    wchar_t path[MAX_PATH];
    DWORD length = GetModuleFileNameW(dllInstance, path, MAX_PATH);
    if (!length || length >= MAX_PATH) return SELFREG_E_CLASS;
    HKEY key = nullptr, server = nullptr;
    if (RegCreateKeyExW(HKEY_LOCAL_MACHINE, CLSID_KEY, 0, nullptr, 0, KEY_WRITE, nullptr, &key, nullptr) != ERROR_SUCCESS) return SELFREG_E_CLASS;
    RegSetValueExW(key, nullptr, 0, REG_SZ, (const BYTE *)DESCRIPTION, sizeof(DESCRIPTION));
    LONG r = RegCreateKeyExW(key, L"InprocServer32", 0, nullptr, 0, KEY_WRITE, nullptr, &server, nullptr);
    if (r == ERROR_SUCCESS) {
        RegSetValueExW(server, nullptr, 0, REG_SZ, (const BYTE *)path, (length + 1) * sizeof(wchar_t));
        RegSetValueExW(server, L"ThreadingModel", 0, REG_SZ, (const BYTE *)L"Apartment", sizeof(L"Apartment"));
        RegCloseKey(server);
    }
    RegCloseKey(key);
    if (r != ERROR_SUCCESS) return SELFREG_E_CLASS;

    ITfInputProcessorProfileMgr *profiles = nullptr;
    ITfCategoryMgr *categories = nullptr;
    HRESULT hr = CoCreateInstance(CLSID_TF_InputProcessorProfiles, nullptr, CLSCTX_INPROC_SERVER, IID_ITfInputProcessorProfileMgr, (void **)&profiles);
    if (SUCCEEDED(hr)) hr = CoCreateInstance(CLSID_TF_CategoryMgr, nullptr, CLSCTX_INPROC_SERVER, IID_ITfCategoryMgr, (void **)&categories);
    for (LANGID language : LANGUAGES) {
        if (SUCCEEDED(hr)) hr = profiles->RegisterProfile(CLSID_KotoTypeTip, language, PROFILE_KotoTypeTip, DESCRIPTION, ARRAYSIZE(DESCRIPTION) - 1, path, length, 0, nullptr, 0, TRUE, 0);
    }
    for (const GUID *category : CATEGORIES) {
        if (SUCCEEDED(hr)) hr = categories->RegisterCategory(CLSID_KotoTypeTip, *category, CLSID_KotoTypeTip);
    }
    if (categories) categories->Release();
    if (profiles) profiles->Release();
    if (FAILED(hr)) DllUnregisterServer();
    return hr;
}

STDAPI DllUnregisterServer() {
    ITfInputProcessorProfileMgr *profiles = nullptr;
    ITfCategoryMgr *categories = nullptr;
    if (SUCCEEDED(CoCreateInstance(CLSID_TF_CategoryMgr, nullptr, CLSCTX_INPROC_SERVER, IID_ITfCategoryMgr, (void **)&categories))) {
        for (const GUID *category : CATEGORIES) categories->UnregisterCategory(CLSID_KotoTypeTip, *category, CLSID_KotoTypeTip);
        categories->Release();
    }
    if (SUCCEEDED(CoCreateInstance(CLSID_TF_InputProcessorProfiles, nullptr, CLSCTX_INPROC_SERVER, IID_ITfInputProcessorProfileMgr, (void **)&profiles))) {
        for (LANGID language : LANGUAGES) profiles->UnregisterProfile(CLSID_KotoTypeTip, language, PROFILE_KotoTypeTip, 0);
        profiles->Release();
    }
    RegDeleteTreeW(HKEY_LOCAL_MACHINE, CLSID_KEY);
    // UnregisterProfile leaves the empty TIP key behind.
    RegDeleteTreeW(HKEY_LOCAL_MACHINE, TIP_KEY);
    return S_OK;
}
