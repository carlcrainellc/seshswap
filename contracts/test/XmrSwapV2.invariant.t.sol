// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV2} from "../src/XmrSwapV2.sol";
import {RejectEth} from "./Tokens.sol";

/// Random sequences of lock / setReady / claim / claimRelayed / refund / withdraw / warp.
contract Handler is Test {
    XmrSwapV2 public c;
    uint256 constant BOB_S = 0x088fba2ab5511efb59a49f0f8e1ceceeb302ad1f60a4007702607c789719eaf5;
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    uint256 constant BOB_PK = 0xB0B5EC;
    address public alice = address(0xA11CE);
    address public bob;
    address public rejecter;

    XmrSwapV2.Swap[] public swaps;
    mapping(bytes32 => uint8) public lastStatus;
    uint256 public ghostLocked;
    uint256 public ghostPaidOut;     // pushed + withdrawn
    uint256 public ghostOwed;
    bool public terminalChanged;
    bool public doubleAction;
    uint256 salt;
    uint256 public nClaims;
    uint256 public nRefunds;

    constructor(XmrSwapV2 c_) {
        c = c_;
        bob = vm.addr(BOB_PK);
        rejecter = address(new RejectEth());
        vm.deal(alice, 1e30);
    }

    function count() external view returns (uint256) { return swaps.length; }
    function swapAt(uint256 i) external view returns (XmrSwapV2.Swap memory) { return swaps[i]; }

    function lock(uint256 value, uint64 d1, uint64 d2) external {
        value = bound(value, 1, 1e24);
        d1 = uint64(bound(d1, c.MIN_D1(), 30 days));
        d2 = uint64(bound(d2, c.MIN_D2(), 30 days));
        salt++;
        uint64 t1 = uint64(block.timestamp) + d1;
        XmrSwapV2.Swap memory s = XmrSwapV2.Swap(alice, bob, BOB_K, ALICE_K, t1, t1 + d2, address(0), value, bytes32(salt));
        vm.prank(alice);
        c.lock{value: value}(bob, BOB_K, ALICE_K, d1, d2, address(0), value, bytes32(salt));
        swaps.push(s);
        lastStatus[c.swapId(s)] = 1;
        ghostLocked += value;
    }

    function _pick(uint256 i) internal view returns (XmrSwapV2.Swap memory s, bytes32 id, bool ok) {
        if (swaps.length == 0) return (s, id, false);
        s = swaps[i % swaps.length];
        id = c.swapId(s);
        ok = true;
    }

    function _after(bytes32 id, uint8 before) internal {
        uint8 now_ = uint8(c.status(id));
        if (before >= 3 && now_ != before) terminalChanged = true;
        lastStatus[id] = now_;
    }

    function warp(uint256 dt) external { vm.warp(block.timestamp + bound(dt, 0, 10 days)); }

    function setReady(uint256 i) external {
        (XmrSwapV2.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        uint8 b = uint8(c.status(id));
        vm.prank(alice);
        try c.setReady(s) {} catch {}
        _after(id, b);
    }

    function claim(uint256 i, bool toRejecter) external {
        (XmrSwapV2.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        uint8 b = uint8(c.status(id));
        address to = toRejecter ? rejecter : bob;
        uint256 bal = bob.balance;
        vm.prank(bob);
        try c.claim(s, BOB_S, to) {
            if (b >= 3) doubleAction = true;
            nClaims++;
            if (toRejecter) ghostOwed += s.value; else { ghostPaidOut += s.value; assertEq(bob.balance - bal, s.value); }
        } catch {}
        _after(id, b);
    }

    function claimRelayed(uint256 i, uint256 fee) external {
        (XmrSwapV2.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        fee = bound(fee, 0, s.value);
        uint8 b = uint8(c.status(id));
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, c.claimDigest(id, bob, address(0), fee));
        vm.prank(address(0x7E1A7));
        try c.claimRelayed(s, BOB_S, bob, address(0), fee, v, r, ss) {
            if (b >= 3) doubleAction = true;
            nClaims++;
            ghostPaidOut += s.value;
        } catch {}
        _after(id, b);
    }

    function refund(uint256 i) external {
        (XmrSwapV2.Swap memory s, bytes32 id, bool ok) = _pick(i);
        if (!ok) return;
        uint8 b = uint8(c.status(id));
        vm.prank(alice);
        try c.refund(s, ALICE_S) {
            nRefunds++;
            if (b >= 3) doubleAction = true;
            ghostPaidOut += s.value;
        } catch {}
        _after(id, b);
    }

    function withdrawRejecter() external {
        uint256 o = c.owed(address(0), rejecter);
        if (o == 0) return;
        vm.prank(rejecter);
        c.withdraw(address(0), bob);
        ghostOwed -= o;
        ghostPaidOut += o;
    }

    function activeValue() external view returns (uint256 sum) {
        for (uint256 i = 0; i < swaps.length; i++) {
            uint8 st = uint8(c.status(c.swapId(swaps[i])));
            if (st == 1 || st == 2) sum += swaps[i].value;
        }
    }
}

contract XmrSwapV2Invariants is Test {
    XmrSwapV2 c;
    Handler h;

    function setUp() public {
        vm.warp(1_000_000);
        c = new XmrSwapV2();
        h = new Handler(c);
        targetContract(address(h));
    }

    function afterInvariant() public {
        emit log_named_uint("claims", h.nClaims());
        emit log_named_uint("refunds", h.nRefunds());
    }

    /// Contract always holds exactly: value of open swaps + deferred payouts.
    function invariant_Solvency() public view {
        assertEq(address(c).balance, h.activeValue() + c.owed(address(0), h.rejecter()));
        assertEq(h.ghostOwed(), c.owed(address(0), h.rejecter()));
    }

    /// Conservation: everything locked is either still locked, owed, or paid out once.
    function invariant_Conservation() public view {
        assertEq(h.ghostLocked(), h.activeValue() + h.ghostOwed() + h.ghostPaidOut());
    }

    /// A finished swap never changes again and is never paid twice.
    function invariant_TerminalIsFinal() public view {
        assertTrue(!h.terminalChanged());
        assertTrue(!h.doubleAction());
    }
}
