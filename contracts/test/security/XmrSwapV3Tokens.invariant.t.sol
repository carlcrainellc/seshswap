// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {Token, FalseToken, UsdtLike, RejectEth} from "../Tokens.sol";

/// Token that re-enters XmrSwapV3 on every transfer / transferFrom and records any success.
contract EvilToken is Token {
    XmrSwapV3 public v3;
    bytes public payload;
    bool public reentrySucceeded;
    function setTarget(XmrSwapV3 v, bytes calldata p) external { v3 = v; payload = p; }
    function _poke() internal {
        if (address(v3) == address(0)) return;
        (bool ok,) = address(v3).call(payload);
        if (ok) reentrySucceeded = true;
    }
    function transfer(address t, uint256 v) external override returns (bool) {
        _poke();
        _move(msg.sender, t, v);
        return true;
    }
}

/// Swap owner that is a contract and rejects ETH (refund/premium must be deferred, not lost).
contract OwnerContract {
    XmrSwapV3 public v3;
    constructor(XmrSwapV3 v) { v3 = v; }
    function lock(address claimer, uint256 ck, uint256 rk, uint64 g, address asset, uint256 value, uint256 premium, bytes32 salt)
        external payable returns (bytes32)
    {
        if (asset != address(0)) {
            (bool ok,) = asset.call(abi.encodeWithSignature("approve(address,uint256)", address(v3), value));
            require(ok);
        }
        return v3.lock{value: msg.value}(claimer, ck, rk, 1 hours, 1 hours, g, asset, value, premium, salt);
    }
    function cancel(XmrSwapV3.Swap calldata s) external { v3.cancelIntent(s); }
    function ready(XmrSwapV3.Swap calldata s) external { v3.setReady(s); }
    function refund(XmrSwapV3.Swap calldata s, uint256 secret) external { v3.refund(s, secret); }
    function pull(address asset, address to) external { v3.withdraw(asset, to); }
    receive() external payable { revert("no eth"); }
}

/// XmrSwapV3 with ERC-20s (plain, false-returning, USDT-style, blacklisting,
/// re-entering, fee-on-transfer), a contract owner that rejects ETH, and an
/// attacker that tries wrong secrets, wrong callers and forged signatures.
contract V3TokenHandler is Test {
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint256 constant BOB_PK = 0xB0B5EC;
    uint256 constant EVE_PK = 0xE7E;

    XmrSwapV3 public c;
    address[] public assets; // [0]=ETH, then tokens
    Token public plain;
    FalseToken public falsy;
    UsdtLike public usdt;
    EvilToken public evil;
    Token public feeTok;
    OwnerContract public oc;
    address public alice = address(0xA11CE);
    address public bob;
    address public eve; // attacker
    address public rejecter;
    address[] public payees;

    struct S { XmrSwapV3.Swap s; bool viaContract; }
    S[] internal swaps;

    bool public stolen;          // value reached someone other than the rightful party
    bool public badSecretWorked;
    bool public wrongCallerWorked;
    bool public feeLockAccepted;
    bool public terminalChanged;
    bool public cancelInLockIn;  // a cancel intent succeeded inside the lock-in window
    uint256 public nClaims;
    uint256 public nRefunds;
    uint256 public nDeferred;
    uint256 salt;

    constructor(XmrSwapV3 c_) {
        c = c_;
        bob = vm.addr(BOB_PK);
        eve = vm.addr(EVE_PK);
        rejecter = address(new RejectEth());
        plain = new Token();
        falsy = new FalseToken();
        usdt = new UsdtLike();
        evil = new EvilToken();
        feeTok = new Token();
        oc = new OwnerContract(c_);
        assets.push(address(0));
        assets.push(address(plain));
        assets.push(address(falsy));
        assets.push(address(usdt));
        assets.push(address(evil));
        vm.deal(alice, 1e30);
        vm.deal(address(oc), 0);
        vm.deal(eve, 1e30);
        for (uint256 i = 1; i < assets.length; i++) {
            Token(assets[i]).mint(alice, 1e30);
            Token(assets[i]).mint(address(oc), 1e30);
            vm.prank(alice);
            (bool ok,) = assets[i].call(abi.encodeWithSignature("approve(address,uint256)", address(c_), type(uint256).max));
            require(ok);
        }
        feeTok.mint(alice, 1e30);
        vm.prank(alice);
        feeTok.approve(address(c_), type(uint256).max);
        payees.push(bob);
        payees.push(rejecter);
        payees.push(address(0xF2E5));
    }

    function nSwaps() external view returns (uint256) { return swaps.length; }
    function swapAt(uint256 i) external view returns (XmrSwapV3.Swap memory) { return swaps[i].s; }

    function _st(bytes32 id) internal view returns (uint8 st, uint64 d) {
        (XmrSwapV3.Stage s, uint64 dl,) = c.status(id);
        return (uint8(s), dl);
    }

    function warp(uint256 dt) external { vm.warp(block.timestamp + bound(dt, 0, 2 days)); }

    /// Jump to an exact boundary of a swap (t1, t2 or the freeze deadline), +/- 1s.
    function warpEdge(uint256 i, uint8 which, uint8 off) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        (, uint64 d) = _st(c.swapId(s));
        uint64 t = which % 3 == 0 ? s.t1 : which % 3 == 1 ? s.t2 : d;
        if (t == 0) return;
        uint64 target = t + uint64(off % 3) - 1; // t-1, t, t+1
        if (target > block.timestamp) vm.warp(target);
    }

    function lock(uint256 a, uint256 value, uint256 premium, uint64 grace, bool viaContract) external {
        XmrSwapV3.Swap memory s;
        s.asset = assets[a % assets.length];
        s.value = bound(value, 1, 1e24);
        s.premium = bound(premium, 0, 1e20);
        s.grace = uint64(bound(grace, 30 minutes, 48 hours));
        s.salt = bytes32(++salt);
        s.owner = viaContract ? address(oc) : alice;
        s.claimer = bob;
        s.claimKey = BOB_K;
        s.refundKey = ALICE_K;
        s.t1 = uint64(block.timestamp) + 1 hours;
        s.t2 = s.t1 + 1 hours;
        if (_doLock(s, viaContract)) swaps.push(S(s, viaContract));
    }

    function _doLock(XmrSwapV3.Swap memory s, bool viaContract) internal returns (bool ok) {
        uint256 msgv = s.asset == address(0) ? s.value + s.premium : s.premium;
        if (viaContract) {
            vm.deal(address(this), msgv);
            try oc.lock{value: msgv}(bob, BOB_K, ALICE_K, s.grace, s.asset, s.value, s.premium, s.salt) { ok = true; } catch {}
        } else {
            vm.prank(alice);
            try c.lock{value: msgv}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, s.grace, s.asset, s.value, s.premium, s.salt) {
                ok = true;
            } catch {}
        }
    }

    function lockFeeToken(uint256 value, uint256 bps) external {
        feeTok.setFee(bound(bps, 1, 1000));
        value = bound(value, 10_000, 1e24);
        vm.prank(alice);
        try c.lock(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 30 minutes, address(feeTok), value, 0, bytes32(++salt)) {
            feeLockAccepted = true;
        } catch {}
        feeTok.setFee(0);
    }

    // token mischief between steps
    function toggleFalse(bool b) external { falsy.setFail(b); }
    function toggleBlock(uint256 a, uint256 p, bool b) external {
        address asset = assets[a % assets.length];
        if (asset == address(0) || asset == address(usdt)) return;
        Token(asset).setBlocked(payees[p % payees.length], b);
    }
    function armEvil(uint256 i, uint8 op) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        bytes memory p;
        if (op % 4 == 0) p = abi.encodeCall(XmrSwapV3.withdraw, (address(evil), address(evil)));
        else if (op % 4 == 1) p = abi.encodeCall(XmrSwapV3.refund, (s, ALICE_S));
        else if (op % 4 == 2) p = abi.encodeCall(XmrSwapV3.claim, (s, BOB_S, address(evil)));
        else p = abi.encodeCall(XmrSwapV3.lock, (bob, BOB_K, ALICE_K, 1 hours, 1 hours, 30 minutes, address(0), 1, 0, bytes32(++salt)));
        evil.setTarget(c, p);
    }

    function _owedSnapshot(address asset, address who) internal view returns (uint256) { return c.owed(asset, who); }

    function _bal(address asset, address who) internal view returns (uint256) {
        if (asset == address(0)) return who.balance;
        return Token(asset).balanceOf(who);
    }

    function ready(uint256 i) external {
        if (swaps.length == 0) return;
        S memory x = swaps[i % swaps.length];
        if (x.viaContract) { try oc.ready(x.s) {} catch {} }
        else { vm.prank(alice); try c.setReady(x.s) {} catch {} }
    }

    function cancel(uint256 i) external {
        if (swaps.length == 0) return;
        S memory x = swaps[i % swaps.length];
        bool inWin = block.timestamp < x.s.t1 - 1 hours + c.LOCK_IN(); // lock time = t1 - 1h
        if (x.viaContract) { try oc.cancel(x.s) { if (inWin) cancelInLockIn = true; } catch {} }
        else { vm.prank(alice); try c.cancelIntent(x.s) { if (inWin) cancelInLockIn = true; } catch {} }
    }

    function intent(uint256 i, bool forged) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        bytes32 id = c.swapId(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(forged ? EVE_PK : BOB_PK, c.intentDigest(id));
        vm.prank(eve);
        try c.claimIntent(s, v, r, ss) { if (forged) wrongCallerWorked = true; } catch {}
    }

    uint256 private _b0;
    uint256 private _o0;
    uint8 private _stage0;

    function claim(uint256 i, uint256 p, uint256 secret, bool asEve) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        (_stage0,) = _st(c.swapId(s));
        address to = payees[p % payees.length];
        uint256 sec = secret % 4 != 0 ? BOB_S : secret;
        _b0 = _bal(s.asset, to);
        _o0 = c.owed(s.asset, to);
        vm.prank(asEve ? eve : bob);
        try c.claim(s, sec, to) {
            _afterClaim(s, to, sec, asEve);
        } catch {}
    }

    function _afterClaim(XmrSwapV3.Swap memory s, address to, uint256 sec, bool asEve) internal {
        nClaims++;
        if (asEve) wrongCallerWorked = true;
        if (sec != BOB_S) badSecretWorked = true;
        if (_stage0 == 3 || _stage0 == 4) terminalChanged = true;
        uint256 got = _bal(s.asset, to) - _b0 + c.owed(s.asset, to) - _o0;
        if (got != s.value) stolen = true;
        if (c.owed(s.asset, to) > _o0) nDeferred++;
    }

    /// Intent then reveal in one handler step (otherwise claims are rare).
    function intentAndClaim(uint256 i, uint256 p) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        bytes32 id = c.swapId(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, c.intentDigest(id));
        try c.claimIntent(s, v, r, ss) {} catch {}
        (_stage0,) = _st(id);
        address to = payees[p % payees.length];
        _b0 = _bal(s.asset, to);
        _o0 = c.owed(s.asset, to);
        vm.prank(bob);
        try c.claim(s, BOB_S, to) { _afterClaim(s, to, BOB_S, false); } catch {}
    }

    function claimRelayed(uint256 i, uint256 fee, bool forged, bool redirect) external {
        if (swaps.length == 0) return;
        XmrSwapV3.Swap memory s = swaps[i % swaps.length].s;
        bytes32 id = c.swapId(s);
        fee = bound(fee, 0, s.value);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(forged ? EVE_PK : BOB_PK, c.claimDigest(id, bob, address(0), fee));
        vm.prank(eve);
        // redirect: Eve submits Bob's signature but names herself as recipient
        try c.claimRelayed(s, BOB_S, redirect ? eve : bob, address(0), fee, v, r, ss) {
            nClaims++;
            if (forged || redirect) wrongCallerWorked = true;
        } catch {}
    }

    function refund(uint256 i, uint256 secret, bool asEve) external {
        if (swaps.length == 0) return;
        S memory x = swaps[i % swaps.length];
        bytes32 id = c.swapId(x.s);
        (uint8 b,) = _st(id);
        uint256 sec = secret % 4 != 0 ? ALICE_S : secret;
        bool ok;
        if (asEve) {
            vm.prank(eve);
            try c.refund(x.s, sec) { ok = true; wrongCallerWorked = true; } catch {}
        } else if (x.viaContract) {
            try oc.refund(x.s, sec) { ok = true; } catch {}
        } else {
            vm.prank(alice);
            try c.refund(x.s, sec) { ok = true; } catch {}
        }
        if (ok) {
            nRefunds++;
            if (sec != ALICE_S) badSecretWorked = true;
            if (b == 3 || b == 4) terminalChanged = true;
        }
    }

    function withdrawAll(uint256 a, uint256 p) external {
        address asset = assets[a % assets.length];
        address who = payees[p % payees.length];
        if (c.owed(asset, who) == 0) return;
        vm.prank(who);
        try c.withdraw(asset, address(0xF2E5)) {} catch {}
    }

    function withdrawOc(uint256 a) external {
        address asset = assets[a % assets.length];
        if (c.owed(asset, address(oc)) == 0) return;
        try oc.pull(asset, address(0xF2E5)) {} catch {}
    }

    /// Eve pulls: she may only ever get what she is owed (relayer fees), never anyone else's.
    function eveWithdraw(uint256 a) external {
        address asset = assets[a % assets.length];
        uint256 before = _bal(asset, eve);
        uint256 owedEve = c.owed(asset, eve); // only relayer fees she earned
        vm.prank(eve);
        try c.withdraw(asset, eve) {
            if (_bal(asset, eve) - before != owedEve) stolen = true;
        } catch {}
    }

    // ---------------------------------------------------------------- views
    function obligations(address asset) external view returns (uint256 sum) {
        for (uint256 i = 0; i < swaps.length; i++) {
            XmrSwapV3.Swap memory s = swaps[i].s;
            (uint8 st,) = _st(c.swapId(s));
            if (st == 3 || st == 4) continue;
            if (s.asset == asset) sum += s.value;
            if (asset == address(0)) sum += s.premium;
        }
        for (uint256 p = 0; p < payees.length; p++) sum += c.owed(asset, payees[p]);
        sum += c.owed(asset, address(oc)) + c.owed(asset, alice) + c.owed(asset, eve) + c.owed(asset, address(evil));
        sum += c.owed(asset, address(this));
    }

    function nAssets() external view returns (uint256) { return assets.length; }
}

contract XmrSwapV3TokenInvariants is Test {
    XmrSwapV3 c;
    V3TokenHandler h;

    function setUp() public {
        vm.warp(1_000_000);
        c = new XmrSwapV3();
        h = new V3TokenHandler(c);
        targetContract(address(h));
    }

    function afterInvariant() public {
        if (vm.envOr("SEC_STATS", false)) {
            vm.writeLine("fuzz-stats/v3tokens.csv", string.concat(
                vm.toString(h.nSwaps()), ",", vm.toString(h.nClaims()), ",", vm.toString(h.nRefunds()), ",",
                vm.toString(h.nDeferred())));
        }
    }

    /// For every asset: contract balance == open swap values (+ premiums for ETH) + owed.
    function invariant_PerAssetSolvency() public view {
        for (uint256 i = 0; i < h.nAssets(); i++) {
            address a = h.assets(i);
            uint256 bal = a == address(0) ? address(c).balance : V3Bal(a).balanceOf(address(c));
            assertEq(bal, h.obligations(a));
        }
    }

    function invariant_NoTheft() public view {
        assertTrue(!h.stolen(), "payout to the wrong party / wrong amount");
        assertTrue(!h.badSecretWorked(), "wrong secret accepted");
        assertTrue(!h.wrongCallerWorked(), "wrong caller / forged sig / redirect accepted");
    }

    function invariant_FeeOnTransferRejected() public view { assertTrue(!h.feeLockAccepted()); }

    function invariant_TerminalFinal() public view { assertTrue(!h.terminalChanged()); }

    function invariant_NoCancelInLockIn() public view { assertTrue(!h.cancelInLockIn()); }

    function invariant_ReentryNeverSucceeds() public view { assertTrue(!h.evil().reentrySucceeded()); }
}

interface V3Bal { function balanceOf(address) external view returns (uint256); }
